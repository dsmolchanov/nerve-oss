package jmap

import "neuralmail/internal/config"

func NewClient(cfg config.Config) (Client, error) {
	if cfg.JMAP.URL == "" || cfg.JMAP.Username == "" || cfg.JMAP.Password == "" {
		return NoopClient{}, nil
	}
	return NoopClient{}, nil
}
