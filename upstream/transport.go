package upstream

import (
	"net/http"

	"antiapi/transport"
)

type StealthTransport = transport.StealthTransport

func NewStealthTransport() http.RoundTripper {
	return transport.GetSharedTransport()
}

func BuildUserAgent() string {
	return transport.BuildUserAgent()
}
