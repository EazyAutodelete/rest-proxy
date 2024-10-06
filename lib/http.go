package lib

import (
	"io"
	"net/http"
	"strings"
)

func copyHeader(dst, src http.Header) {
	dst["Date"] = nil
	dst["Content-Type"] = nil
	for k, vv := range src {
		for _, v := range vv {
			if k != "Content-Length" {
				dst[strings.ToLower(k)] = []string{v}
			}
		}
	}
}

func CopyResponseToResponseWriter(resp *http.Response, response *Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		response.SetStatus(500)
		response.WriteBody([]byte(err.Error()))
		return err
	}

	copyHeader(response.Header(), resp.Header)
	response.SetStatus(resp.StatusCode)

	response.WriteBody(body)

	return nil
}
