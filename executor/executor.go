package executor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	_ "encoding/json"
	"fmt"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/bnb-chain/greenfield-go-sdk/client/sp"
	"github.com/tendermint/tendermint/votepool"

	"github.com/bnb-chain/greenfield-challenger/config"
	"github.com/bnb-chain/greenfield-challenger/logging"
	sdkclient "github.com/bnb-chain/greenfield-go-sdk/client/chain"
	sdkkeys "github.com/bnb-chain/greenfield-go-sdk/keys"
	challangetypes "github.com/bnb-chain/greenfield/x/challenge/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/tendermint/tendermint/rpc/client"
	coretypes "github.com/tendermint/tendermint/rpc/core/types"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tmtypes "github.com/tendermint/tendermint/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Executor struct {
	gnfdClients *sdkclient.GnfdCompositeClients
	spClient    *sp.SPClient
	config      *config.Config
	address     string
	validators  []*tmtypes.Validator // used to cache validators
}

func NewExecutor(cfg *config.Config) *Executor {
	privKey := getGreenfieldPrivateKey(&cfg.GreenfieldConfig)

	km, err := sdkkeys.NewPrivateKeyManager(privKey)
	if err != nil {
		panic(err)
	}

	clients := sdkclient.NewGnfdCompositClients(
		cfg.GreenfieldConfig.GRPCAddrs,
		cfg.GreenfieldConfig.RPCAddrs,
		cfg.GreenfieldConfig.ChainIdString,
		sdkclient.WithKeyManager(km),
		sdkclient.WithGrpcDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	spClient, err := sp.NewSpClient("", sp.WithKeyManager(km))
	return &Executor{
		gnfdClients: clients,
		spClient:    spClient,
		address:     km.GetAddr().String(),
		config:      cfg,
	}
}

func getGreenfieldPrivateKey(cfg *config.GreenfieldConfig) string {
	var privateKey string
	if cfg.KeyType == config.KeyTypeAWSPrivateKey {
		result, err := config.GetSecret(cfg.AWSSecretName, cfg.AWSRegion)
		if err != nil {
			panic(err)
		}
		type AwsPrivateKey struct {
			PrivateKey string `json:"private_key"`
		}
		var awsPrivateKey AwsPrivateKey
		err = json.Unmarshal([]byte(result), &awsPrivateKey)
		if err != nil {
			panic(err)
		}
		privateKey = awsPrivateKey.PrivateKey
	} else {
		privateKey = cfg.PrivateKey
	}
	return privateKey
}

func (e *Executor) getRpcClient() (client.Client, error) {
	client, err := e.gnfdClients.GetClient()
	if err != nil {
		return nil, err
	}
	return client.TendermintClient.RpcClient.TmClient, nil
}

func (e *Executor) GetGnfdClient() (*sdkclient.GreenfieldClient, error) {
	client, err := e.gnfdClients.GetClient()
	if err != nil {
		return nil, err
	}
	return client.GreenfieldClient, nil
}

func (e *Executor) GetSPClient() *sp.SPClient {
	client := e.spClient
	return client
}

func (e *Executor) GetBlockAndBlockResultAtHeight(height int64) (*tmtypes.Block, *ctypes.ResultBlockResults, error) {
	client, err := e.getRpcClient()
	if err != nil {
		return nil, nil, err
	}
	block, err := client.Block(context.Background(), &height)
	if err != nil {
		return nil, nil, err
	}
	blockResults, err := client.BlockResults(context.Background(), &height)
	if err != nil {
		return nil, nil, err
	}
	return block.Block, blockResults, nil
}

func (e *Executor) GetLatestBlockHeight() (latestHeight uint64, err error) {
	client, err := e.gnfdClients.GetClient()
	if err != nil {
		return 0, err
	}
	return uint64(client.Height), nil
}

func (e *Executor) queryLatestValidators() ([]*tmtypes.Validator, error) {
	client, err := e.getRpcClient()
	if err != nil {
		return nil, err
	}
	validators, err := client.Validators(context.Background(), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return validators.Validators, nil
}

func (e *Executor) QueryCachedLatestValidators() ([]*tmtypes.Validator, error) {
	if len(e.validators) != 0 {
		return e.validators, nil
	}
	validators, err := e.queryLatestValidators()
	if err != nil {
		return nil, err
	}
	return validators, nil
}

func (e *Executor) UpdateCachedLatestValidatorsLoop() {
	ticker := time.NewTicker(UpdateCachedValidatorsInterval)
	for range ticker.C {
		validators, err := e.queryLatestValidators()
		if err != nil {
			logging.Logger.Errorf("update latest greenfield validators error, err=%s", err)
			continue
		}
		e.validators = validators
	}
}

func (e *Executor) GetValidatorsBlsPublicKey() ([]string, error) {
	validators, err := e.QueryCachedLatestValidators()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, v := range validators {
		keys = append(keys, hex.EncodeToString(v.RelayerBlsKey))
	}
	return keys, nil
}

func (e *Executor) SendAttestTx(challengeId uint64, objectId, spOperatorAddress string,
	voteResult challangetypes.VoteResult, challenger string,
	voteAddressSet []uint64, aggregatedSig []byte) (string, error) {
	gnfdClient, err := e.GetGnfdClient()
	if err != nil {
		return "", err
	}

	acc, err := sdk.AccAddressFromHexUnsafe(e.address)
	if err != nil {
		return "", err
	}

	msgHeartbeat := challangetypes.NewMsgAttest(
		acc,
		challengeId,
		sdkmath.NewUintFromString(objectId),
		spOperatorAddress,
		challangetypes.VoteResult(voteResult),
		challenger,
		voteAddressSet,
		aggregatedSig,
	)

	txRes, err := gnfdClient.BroadcastTx(
		[]sdk.Msg{msgHeartbeat},
		nil,
	)
	if err != nil {
		return "", err
	}
	if txRes.TxResponse.Code != 0 {
		return "", fmt.Errorf("tx error, code=%d, log=%s", txRes.TxResponse.Code, txRes.TxResponse.RawLog)
	}
	return txRes.TxResponse.TxHash, nil
}

func (e *Executor) QueryLatestAttestedChallenge() (uint64, error) {
	client, err := e.gnfdClients.GetClient()
	if err != nil {
		return 0, err
	}

	res, err := client.ChallengeQueryClient.LatestAttestedChallenge(context.Background(), &challangetypes.QueryLatestAttestedChallengeRequest{})
	if err != nil {
		return 0, err
	}

	return res.ChallengeId, nil
}

func (e *Executor) QueryVotes(eventHash []byte, eventType votepool.EventType) ([]*votepool.Vote, error) {
	client, err := e.gnfdClients.GetClient()
	if err != nil {
		return nil, err
	}

	queryMap := make(map[string]interface{})
	queryMap[VotePoolQueryParameterEventType] = int(eventType)
	queryMap[VotePoolQueryParameterEventHash] = eventHash
	var queryVote coretypes.ResultQueryVote
	_, err = client.JsonRpcClient.Call(context.Background(), VotePoolQueryMethodName, queryMap, &queryVote)
	if err != nil {
		return nil, err
	}
	return queryVote.Votes, nil
}

func (e *Executor) BroadcastVote(v *votepool.Vote) error {
	client, err := e.gnfdClients.GetClient()
	if err != nil {
		return err
	}
	broadcastMap := make(map[string]interface{})
	broadcastMap[VotePoolBroadcastParameterKey] = *v
	_, err = client.JsonRpcClient.Call(context.Background(), VotePoolBroadcastMethodName, broadcastMap, &ctypes.ResultBroadcastVote{})
	if err != nil {
		return err
	}
	return nil
}
