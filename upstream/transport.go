package upstream

import (
	"net/http"

	"antiapi/transport"
)

type StealthTransport = transport.StealthTransport

func NewStealthTransport() http.RoundTripper {
	return transport.NewStealthTransport()
}

func BuildUserAgent() string {
	return transport.BuildUserAgent()
}
