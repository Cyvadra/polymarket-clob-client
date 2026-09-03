// Package polymarket groups the CLOB, Gamma, and Data API clients.
package polymarket

import (
	clobclient "github.com/Cyvadra/polymarket-clob-client"
	"github.com/Cyvadra/polymarket-clob-client/data"
	"github.com/Cyvadra/polymarket-clob-client/gamma"
)

type Config struct {
	CLOB  clobclient.Config
	Gamma gamma.Config
	Data  data.Config
}

type Client struct {
	CLOB  *clobclient.Client
	Gamma *gamma.Client
	Data  *data.Client
}

func New(cfg Config) (*Client, error) {
	clob, err := clobclient.New(cfg.CLOB)
	if err != nil {
		return nil, err
	}
	gammaClient, err := gamma.New(cfg.Gamma)
	if err != nil {
		return nil, err
	}
	dataClient, err := data.New(cfg.Data)
	if err != nil {
		return nil, err
	}
	return &Client{CLOB: clob, Gamma: gammaClient, Data: dataClient}, nil
}
