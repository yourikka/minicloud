// Command echo is the standard Go compatibility fixture for wasi-command-v1.
package main

import (
	"os"

	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func main() {
	request, err := abi.DecodeRequest(os.Stdin, abi.Limits{})
	if err != nil {
		os.Exit(2)
	}
	response := abi.Response{
		SpecVersion: abi.Version,
		Status:      200,
		Headers:     abi.ResponseHeaders{"content-type": {"application/octet-stream"}},
		Body:        request.Body,
	}
	if err := abi.EncodeResponse(os.Stdout, response, request.Method, abi.Limits{}); err != nil {
		os.Exit(3)
	}
}
