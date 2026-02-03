package jmap

import "neuralmail/internal/config"

func NewClient(cfg config.Config) (Client, error) {
	if cfg.JMAP.URL == "" || cfg.JMAP.Username == "" || cfg.JMAP.Password == "" {
		return NoopClient{}, nil
	}
	client, err := NewJMAPClient(cfg)
	if err != nil {
		return NoopClient{}, err
	}
	return client, nil
}
