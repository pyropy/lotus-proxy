package main

import (
	"context"
	"github.com/filecoin-project/go-jsonrpc"
	lotusapi "github.com/filecoin-project/lotus/api"
	"log"
	"net/http"
)

type ProxiedRPCApi struct {
	// TODO: Add other RPC API's
	minerAPI *lotusapi.StorageMinerStruct
	closer   jsonrpc.ClientCloser
}

func NewProxiedRpcAPI(authToken string, addr string) (*ProxiedRPCApi, error) {
	headers := http.Header{"Authorization": []string{"Bearer " + authToken}}
	pushUrl, err := getPushUrl("http://" + addr + "/rpc/v0")
	if err != nil {
		log.Fatalf("connecting with lotus as stream failed: %s", err)
	}

	var workerApi lotusapi.StorageMinerStruct

	closer, err := jsonrpc.NewMergeClient(
		context.Background(),
		"http://"+addr+"/rpc/v0", "Filecoin",
		lotusapi.GetInternalStructs(&workerApi),
		headers,
		append([]jsonrpc.Option{
			jsonrpc.Option(ReaderParamEncoder(pushUrl)),
		})...)

	return &ProxiedRPCApi{
		&workerApi,
		closer,
	}, err
}
