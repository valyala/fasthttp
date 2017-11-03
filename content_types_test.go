package fasthttp

import "testing"

func TestGetContentType(t *testing.T) {
	if s := GetContentType("borja.pdf"); s != "application/pdf" {
		panic("GetContentType don't work")
	}
}
