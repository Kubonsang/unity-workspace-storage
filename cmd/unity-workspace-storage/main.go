package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Kubonsang/unity-workspace-storage/contract"
)

type lifecycle interface {
	Acquire(context.Context, contract.AcquireRequest) (contract.AcquireResponse, error)
	Status(context.Context, contract.StatusRequest) (contract.StatusResponse, error)
	Release(context.Context, contract.ReleaseRequest) (contract.ReleaseResponse, error)
}

type cliError struct {
	SchemaVersion int          `json:"schemaVersion"`
	OK            bool         `json:"ok"`
	Operation     string       `json:"operation,omitempty"`
	Error         cliErrorBody `json:"error"`
}

type cliErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {
	operation, result, err := execute(context.Background(), os.Args[1:], os.Stdin, contract.Default())
	if err != nil {
		writeJSON(os.Stdout, cliError{SchemaVersion: contract.SchemaVersion, OK: false, Operation: operation, Error: cliErrorBody{Code: "workspace-command-failed", Message: err.Error()}})
		os.Exit(1)
	}
	writeJSON(os.Stdout, result)
}

func execute(ctx context.Context, args []string, stdin io.Reader, service lifecycle) (string, any, error) {
	if len(args) < 2 || args[0] != "workspace" {
		return "", nil, errors.New("usage: unity-workspace-storage workspace acquire|status|release")
	}
	operation := args[1]
	switch operation {
	case "acquire":
		flags := newFlagSet("workspace acquire")
		requestPath := flags.String("request", "-", "path to schema-1 acquire JSON, or - for stdin")
		if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
			return operation, nil, errors.Join(err, errors.New("acquire accepts only --request"))
		}
		var request contract.AcquireRequest
		if err := decodeRequest(*requestPath, stdin, &request); err != nil {
			return operation, nil, err
		}
		result, err := service.Acquire(ctx, request)
		return operation, result, err
	case "status":
		flags := newFlagSet("workspace status")
		requestID := flags.String("request-id", "", "optional idempotency request ID")
		if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
			return operation, nil, errors.Join(err, errors.New("status accepts only --request-id"))
		}
		result, err := service.Status(ctx, contract.StatusRequest{SchemaVersion: contract.SchemaVersion, RequestID: *requestID})
		return operation, result, err
	case "release":
		flags := newFlagSet("workspace release")
		requestID := flags.String("request-id", "", "optional idempotency request ID")
		leaseID := flags.String("lease-id", "", "lease returned by workspace acquire")
		if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 || *leaseID == "" {
			return operation, nil, errors.Join(err, errors.New("release requires --lease-id and accepts optional --request-id"))
		}
		result, err := service.Release(ctx, contract.ReleaseRequest{SchemaVersion: contract.SchemaVersion, RequestID: *requestID, LeaseID: *leaseID})
		return operation, result, err
	default:
		return operation, nil, fmt.Errorf("unknown workspace operation %q", operation)
	}
}

func newFlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

func decodeRequest(path string, stdin io.Reader, target any) error {
	reader := stdin
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		reader = file
	}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("decode request: multiple JSON values")
		}
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

func writeJSON(writer io.Writer, value any) {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}
