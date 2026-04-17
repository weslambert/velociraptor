/*
Plugin Kafka.

Velociraptor - Dig Deeper
Copyright (C) 2019-2025 Rapid7 Inc.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sync"
	"time"

	"github.com/Velocidex/ordereddict"
	kafka "github.com/segmentio/kafka-go"

	"www.velocidex.com/golang/velociraptor/acls"
	"www.velocidex.com/golang/velociraptor/artifacts"
	"www.velocidex.com/golang/velociraptor/json"
	vql_subsystem "www.velocidex.com/golang/velociraptor/vql"
	vfilter "www.velocidex.com/golang/vfilter"
	"www.velocidex.com/golang/vfilter/arg_parser"
)

type _KafkaPluginArgs struct {
	Query     vfilter.StoredQuery `vfilter:"required,field=query,doc=Source for rows to upload."`
	Brokers   []string            `vfilter:"required,field=brokers,doc=A list of Kafka broker addresses (host:port)."`
	Topic     string              `vfilter:"required,field=topic,doc=The Kafka topic to publish messages to."`
	ChunkSize int64               `vfilter:"optional,field=chunk_size,doc=Batch this many rows per Kafka write (default 100)."`
	Threads   int64               `vfilter:"optional,field=threads,doc=How many producer threads to run (default 1)."`
	WaitTime  int64               `vfilter:"optional,field=wait_time,doc=Flush incomplete batches after this many seconds (default 2)."`

	ClientCert string `vfilter:"optional,field=client_cert,doc=PEM-encoded client certificate for mTLS."`
	ClientKey  string `vfilter:"optional,field=client_key,doc=PEM-encoded client private key for mTLS."`
	RootCA     string `vfilter:"optional,field=root_ca,doc=PEM-encoded root CA certificate(s) for verifying the broker TLS certificate."`
	SkipVerify bool   `vfilter:"optional,field=skip_verify,doc=Disable TLS certificate verification (insecure)."`

	RequiredAcks int    `vfilter:"optional,field=required_acks,doc=Kafka required-acks: 0=none 1=leader -1=all (default -1)."`
	Compression  string `vfilter:"optional,field=compression,doc=Message compression: none|gzip|snappy|lz4|zstd (default none)."`
}

type _KafkaUploadResponse struct {
	Sent   int
	Errors int
	Error  string
}

type _KafkaPlugin struct{}

func (self _KafkaPlugin) Call(
	ctx context.Context,
	scope vfilter.Scope,
	args *ordereddict.Dict,
) <-chan vfilter.Row {

	output_chan := make(chan vfilter.Row)

	go func() {
		defer close(output_chan)
		defer vql_subsystem.RegisterMonitor(ctx, "kafka", args)()

		if err := vql_subsystem.CheckAccess(scope, acls.NETWORK); err != nil {
			scope.Log("kafka: %v", err)
			return
		}

		arg := &_KafkaPluginArgs{}
		if err := arg_parser.ExtractArgsWithContext(ctx, scope, args, arg); err != nil {
			scope.Log("kafka: %v", err)
			return
		}

		if arg.ChunkSize == 0 {
			arg.ChunkSize = 100
		}
		if arg.Threads == 0 {
			arg.Threads = 1
		}
		if arg.WaitTime == 0 {
			arg.WaitTime = 2
		}
		if arg.RequiredAcks == 0 {
			arg.RequiredAcks = -1
		}

		tlsCfg, err := buildTLSConfig(arg)
		if err != nil {
			scope.Log("kafka: TLS config error: %v", err)
			return
		}

		codec, err := resolveCompression(arg.Compression)
		if err != nil {
			scope.Log("kafka: %v", err)
			return
		}

		config_obj, _ := artifacts.GetConfig(scope)
		_ = config_obj

		row_chan := arg.Query.Eval(ctx, scope)
		var wg sync.WaitGroup
		for i := 0; i < int(arg.Threads); i++ {
			wg.Add(1)
			go kafkaUploadRows(ctx, scope, output_chan, row_chan, tlsCfg, codec, arg, &wg)
		}
		wg.Wait()
	}()

	return output_chan
}

func kafkaUploadRows(
	ctx context.Context,
	scope vfilter.Scope,
	output_chan chan<- vfilter.Row,
	row_chan <-chan vfilter.Row,
	tlsCfg *tls.Config,
	codec kafka.Compression,
	arg *_KafkaPluginArgs,
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	writer := newKafkaWriter(arg, tlsCfg, codec)
	defer func() {
		if err := writer.Close(); err != nil {
			scope.Log("kafka: writer close error: %v", err)
		}
	}()

	batch := make([]kafka.Message, 0, arg.ChunkSize)
	ticker := time.NewTicker(time.Duration(arg.WaitTime) * time.Second)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		resp := &_KafkaUploadResponse{Sent: len(batch)}
		writeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		if err := writer.WriteMessages(writeCtx, batch...); err != nil {
			resp.Errors = len(batch)
			resp.Sent = 0
			resp.Error = err.Error()
			scope.Log("kafka: write error: %v", err)
		}

		select {
		case output_chan <- resp:
		case <-ctx.Done():
		}

		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return

		case <-ticker.C:
			flush()

		case row, ok := <-row_chan:
			if !ok {
				flush()
				return
			}

			msg, err := rowToKafkaMessage(row, arg.Topic)
			if err != nil {
				scope.Log("kafka: serialisation error: %v", err)
				select {
				case output_chan <- &_KafkaUploadResponse{Errors: 1, Error: err.Error()}:
				case <-ctx.Done():
					return
				}
				continue
			}

			batch = append(batch, msg)
			if int64(len(batch)) >= arg.ChunkSize {
				flush()
			}
		}
	}
}

func rowToKafkaMessage(row vfilter.Row, topic string) (kafka.Message, error) {
	dict, ok := row.(*ordereddict.Dict)
	if !ok {
		dict = ordereddict.NewDict()
	}

	data, err := json.Marshal(dict)
	if err != nil {
		return kafka.Message{}, fmt.Errorf("marshal row: %w", err)
	}

	var key []byte
	if artifactName, ok := dict.Get("_artifact"); ok {
		key = []byte(fmt.Sprintf("%v", artifactName))
	}

	return kafka.Message{
		Topic: topic,
		Key:   key,
		Value: data,
		Time:  time.Now(),
	}, nil
}

func newKafkaWriter(arg *_KafkaPluginArgs, tlsCfg *tls.Config, codec kafka.Compression) *kafka.Writer {
	transport := &kafka.Transport{
		TLS:         tlsCfg,
		DialTimeout: 10 * time.Second,
		IdleTimeout: 45 * time.Second,
	}

	return &kafka.Writer{
		Addr:                   kafka.TCP(arg.Brokers...),
		Topic:                  arg.Topic,
		Balancer:               &kafka.LeastBytes{},
		RequiredAcks:           kafka.RequiredAcks(arg.RequiredAcks),
		Compression:            codec,
		Transport:              transport,
		AllowAutoTopicCreation: false,
		BatchSize:              int(arg.ChunkSize),
		BatchTimeout:           time.Duration(arg.WaitTime) * time.Second,
	}
}

func buildTLSConfig(arg *_KafkaPluginArgs) (*tls.Config, error) {
	cfg := &tls.Config{
		InsecureSkipVerify: arg.SkipVerify, // #nosec G402
		MinVersion:         tls.VersionTLS12,
	}

	if arg.RootCA != "" {
		pool, err := parseCertPool(arg.RootCA)
		if err != nil {
			return nil, fmt.Errorf("root_ca: %w", err)
		}
		cfg.RootCAs = pool
	}

	if arg.ClientCert != "" || arg.ClientKey != "" {
		if arg.ClientCert == "" || arg.ClientKey == "" {
			return nil, fmt.Errorf("both client_cert and client_key must be supplied for mTLS")
		}
		cert, err := tls.X509KeyPair([]byte(arg.ClientCert), []byte(arg.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

func parseCertPool(pemData string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	data := []byte(pemData)
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		pool.AddCert(cert)
	}
	if len(pool.Subjects()) == 0 { //nolint:staticcheck
		return nil, fmt.Errorf("no valid certificates found in root_ca PEM")
	}
	return pool, nil
}

func resolveCompression(name string) (kafka.Compression, error) {
	switch name {
	case "", "none":
		return kafka.Compression(0), nil
	case "gzip":
		return kafka.Gzip, nil
	case "snappy":
		return kafka.Snappy, nil
	case "lz4":
		return kafka.Lz4, nil
	case "zstd":
		return kafka.Zstd, nil
	default:
		return 0, fmt.Errorf("unknown compression codec %q; choose none|gzip|snappy|lz4|zstd", name)
	}
}

func (self _KafkaPlugin) Info(scope vfilter.Scope, type_map *vfilter.TypeMap) *vfilter.PluginInfo {
	return &vfilter.PluginInfo{
		Name:    "kafka",
		Doc:     "Upload rows from a query to a Kafka topic using mTLS.",
		ArgType: type_map.AddType(scope, &_KafkaPluginArgs{}),
	}
}

func init() {
	vql_subsystem.RegisterPlugin(&_KafkaPlugin{})
}
