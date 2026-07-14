// Command runtime is the standard Go Worker execution fixture.
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

var invocationCount int

func main() {
	invocationCount++
	request, err := abi.DecodeRequest(os.Stdin, abi.Limits{})
	if err != nil {
		os.Exit(2)
	}
	mode := string(request.Body)
	switch mode {
	case "exit":
		os.Exit(7)
	case "panic":
		panic("fixture panic must not escape the guest boundary")
	case "invalid":
		_, _ = fmt.Fprint(os.Stdout, "not-json")
		return
	case "output":
		chunk := bytes.Repeat([]byte{'x'}, 4096)
		for range 512 {
			_, _ = os.Stdout.Write(chunk)
		}
		return
	case "stderr":
		line := strings.Repeat("L", 20<<10) + "\n"
		for range 16 {
			_, _ = fmt.Fprint(os.Stderr, line)
		}
	case "loop":
		for {
		}
	case "sleep":
		time.Sleep(10 * time.Second)
	}

	_, fileErr := os.ReadFile("/etc/passwd")
	body := fmt.Appendf(
		nil,
		"%d|%d|%d|%t|%t|%s",
		invocationCount,
		len(os.Args),
		len(os.Environ()),
		fileErr != nil,
		request.DeadlineUnixMS > 0,
		mode,
	)
	response := abi.Response{
		SpecVersion: abi.Version,
		Status:      200,
		Headers:     abi.ResponseHeaders{"content-type": {"text/plain"}},
		Body:        body,
	}
	if err := abi.EncodeResponse(os.Stdout, response, request.Method, abi.Limits{}); err != nil {
		os.Exit(3)
	}
}
