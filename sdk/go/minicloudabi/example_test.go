package minicloudabi_test

import (
	"bytes"
	"fmt"

	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func ExampleDecodeRequest() {
	input := bytes.NewBufferString(`{"spec_version":"1.0","invocation_id":"inv_1","method":"POST","path":"/echo","query":{},"headers":{},"body_base64":"aGVsbG8=","deadline_unix_ms":1783771200000,"trigger":{"type":"http","id":"trg_1"}}`)
	request, err := abi.DecodeRequest(input, abi.Limits{})
	if err != nil {
		panic(err)
	}
	fmt.Println(request.InvocationID, request.Method, string(request.Body))
	// Output: inv_1 POST hello
}

func ExampleEncodeResponse() {
	response := abi.Response{
		SpecVersion: abi.Version,
		Status:      200,
		Headers:     abi.ResponseHeaders{"content-type": {"text/plain"}},
		Body:        []byte("ok"),
	}
	var output bytes.Buffer
	if err := abi.EncodeResponse(&output, response, "POST", abi.Limits{}); err != nil {
		panic(err)
	}
	fmt.Println(output.String())
	// Output: {"spec_version":"1.0","status":200,"headers":{"content-type":["text/plain"]},"body_base64":"b2s="}
}
