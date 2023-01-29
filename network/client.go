package network

import (
	"fmt"
	"net"

	"github.com/rs/zerolog"
)

const (
	DefaultSeed = 1000
)

type Client struct {
	net.Conn

	logger zerolog.Logger

	ID                string
	ReceiveBufferSize int
	Network           string // tcp/udp/unix
	Address           string
	// TODO: add read/write deadline and deal with timeouts
}

// TODO: implement a better connection management algorithm

func NewClient(network, address string, receiveBufferSize int, logger zerolog.Logger) *Client {
	var client Client

	client.logger = logger

	// Try to resolve the address and log an error if it can't be resolved
	addr, err := Resolve(network, address, logger)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to resolve address")
	}

	// Create a resolved client
	client = Client{
		Network: network,
		Address: addr,
	}

	// Fall back to the original network and address if the address can't be resolved
	if client.Address == "" || client.Network == "" {
		client = Client{
			Network: network,
			Address: address,
		}
	}

	// Create a new connection
	conn, err := net.Dial(client.Network, client.Address)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to create a new connection")
		return nil
	}

	client.Conn = conn
	if receiveBufferSize <= 0 {
		client.ReceiveBufferSize = DefaultBufferSize
	} else {
		client.ReceiveBufferSize = receiveBufferSize
	}

	logger.Debug().Msgf("New client created: %s", client.Address)
	client.ID = GetID(conn.LocalAddr().Network(), conn.LocalAddr().String(), DefaultSeed, logger)

	return &client
}

func (c *Client) Send(data []byte) error {
	if _, err := c.Write(data); err != nil {
		c.logger.Error().Err(err).Msgf("Couldn't send data to the server: %s", err)
		return fmt.Errorf("couldn't send data to the server: %w", err)
	}
	c.logger.Debug().Msgf("Sent %d bytes to %s", len(data), c.Address)
	return nil
}

func (c *Client) Receive() (int, []byte, error) {
	buf := make([]byte, c.ReceiveBufferSize)
	read, err := c.Read(buf)
	if err != nil {
		c.logger.Error().Err(err).Msgf("Couldn't receive data from the server: %s", err)
		return 0, nil, fmt.Errorf("couldn't receive data from the server: %w", err)
	}
	c.logger.Debug().Msgf("Received %d bytes from %s", read, c.Address)
	return read, buf, nil
}

func (c *Client) Close() {
	c.logger.Debug().Msgf("Closing connection to %s", c.Address)
	if c.Conn != nil {
		c.Conn.Close()
	}
	c.ID = ""
	c.Conn = nil
	c.Address = ""
	c.Network = ""
	c.ReceiveBufferSize = 0
}
