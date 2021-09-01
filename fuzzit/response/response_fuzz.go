//go:build gofuzz
// +build gofuzz

package fuzz

import (
	"bufio"
	"bytes"

	"github.com/valyala/fasthttp"
)

func Fuzz(data []byte) int {
	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(res)

	if err := res.ReadLimitBody(bufio.NewReader(bytes.NewReader(data)), 1024*1024); err != nil {
		return 0
	}

	w := bytes.Buffer{}
	if _, err := res.WriteTo(&w); err != nil {
		return 0
	}

	return 1
}
