package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/Kubonsang/unity-workspace-storage/contract"
	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
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
	operation, result, err := executeTop(context.Background(), os.Args[1:], os.Stdin, contract.Default(), v2.New(defaultV2Client()))
	if err != nil {
		writeJSON(os.Stdout, commandErrorValue(os.Args[1:], operation, result, err))
		os.Exit(1)
	}
	writeJSON(os.Stdout, result)
}

func commandErrorValue(args []string, operation string, result any, err error) any {
	if response, ok := result.(v2.Response); ok {
		response.SchemaVersion = v2.SchemaVersion
		response.OK = false
		if response.Error == nil {
			response.Error = &v2.Error{Code: "workspace-command-failed", Operation: operation, Message: err.Error()}
		}
		return response
	}
	version := contract.SchemaVersion
	if len(args) > 0 && (args[0] == "parent" || args[0] == "serve") {
		version = v2.SchemaVersion
	}
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "--schema" && args[index+1] == "2" {
			version = v2.SchemaVersion
			break
		}
	}
	return cliError{SchemaVersion: version, OK: false, Operation: operation, Error: cliErrorBody{Code: "workspace-command-failed", Message: err.Error()}}
}

func executeTop(ctx context.Context, args []string, stdin io.Reader, legacy lifecycle, modern v2.Service) (string, any, error) {
	// Flag removal below must not mutate os.Args, which commandErrorValue uses
	// to choose the public error envelope after executeTop returns.
	args = append([]string(nil), args...)
	if len(args) > 0 && args[0] == "serve" {
		return "serve", nil, platformServe(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "parent" {
		return executeParentV2(ctx, args[1:], modern)
	}
	if len(args) >= 2 && args[0] == "workspace" {
		schema := 1
		for i := 2; i+1 < len(args); i++ {
			if args[i] == "--schema" {
				value, err := strconv.Atoi(args[i+1])
				if err != nil {
					return args[1], nil, err
				}
				schema = value
				args = append(args[:i], args[i+2:]...)
				break
			}
		}
		if args[1] == "acquire" {
			data, err := readRequestData(args[2:], stdin)
			if err != nil {
				return "acquire", nil, err
			}
			var header struct {
				SchemaVersion int    `json:"schemaVersion"`
				RequestID     string `json:"requestId"`
				Operation     string `json:"operation"`
			}
			if err := json.Unmarshal(data, &header); err != nil {
				return "acquire", nil, err
			}
			schema = header.SchemaVersion
			if schema == 2 {
				var request v2.Request
				if err := decodeBytes(data, &request); err != nil {
					return "acquire", invalidV2Response(header.RequestID, v2.OperationAcquire, err), err
				}
				if request.Operation != "" && request.Operation != v2.OperationAcquire {
					err := errors.New("request operation must be workspace-acquire")
					return "acquire", invalidV2Response(request.RequestID, v2.OperationAcquire, err), err
				}
				response, err := modern.Acquire(ctx, v2.AcquireRequest{RequestID: request.RequestID, ConsumerID: request.ConsumerID, WorkspaceID: request.WorkspaceID, ParentID: request.ParentID, ClientPID: request.ClientPID, Limits: request.Limits})
				return "acquire", response, err
			}
			return execute(ctx, args, bytes.NewReader(data), legacy)
		}
		if schema == 2 {
			return executeWorkspaceV2(ctx, args[1:], modern)
		}
	}
	return execute(ctx, args, stdin, legacy)
}

func executeParentV2(ctx context.Context, args []string, service v2.Service) (string, any, error) {
	if len(args) == 0 {
		return "parent", nil, errors.New("usage: parent begin|commit|abort")
	}
	switch args[0] {
	case "begin":
		flags := newFlagSet("parent begin")
		key := flags.String("compatibility-key", "", "64-character producer compatibility digest")
		requestID := flags.String("request-id", "", "optional idempotency request ID")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *key == "" {
			return "parent-begin", nil, errors.Join(err, errors.New("parent begin requires --compatibility-key"))
		}
		response, err := service.ParentBegin(ctx, v2.ParentBeginRequest{RequestID: *requestID, CompatibilityKey: *key})
		return "parent-begin", response, err
	case "commit", "abort":
		flags := newFlagSet("parent " + args[0])
		transactionID := flags.String("transaction-id", "", "transaction returned by parent begin")
		requestID := flags.String("request-id", "", "optional idempotency request ID")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *transactionID == "" {
			return "parent-" + args[0], nil, errors.Join(err, errors.New("transaction ID is required"))
		}
		if args[0] == "commit" {
			response, err := service.ParentCommit(ctx, v2.ParentCommitRequest{RequestID: *requestID, TransactionID: *transactionID})
			return "parent-commit", response, err
		}
		response, err := service.ParentAbort(ctx, v2.ParentAbortRequest{RequestID: *requestID, TransactionID: *transactionID})
		return "parent-abort", response, err
	default:
		return "parent-" + args[0], nil, fmt.Errorf("unknown parent operation %q", args[0])
	}
}

func executeWorkspaceV2(ctx context.Context, args []string, service v2.Service) (string, any, error) {
	if len(args) == 0 {
		return "workspace", nil, errors.New("usage: workspace status|release --schema 2")
	}
	switch args[0] {
	case "status":
		flags := newFlagSet("workspace status")
		requestID := flags.String("request-id", "", "optional idempotency request ID")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return "status", nil, err
		}
		response, err := service.Status(ctx, v2.StatusRequest{RequestID: *requestID})
		return "status", response, err
	case "release":
		flags := newFlagSet("workspace release")
		requestID := flags.String("request-id", "", "optional idempotency request ID")
		leaseID := flags.String("lease-id", "", "lease returned by acquire")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *leaseID == "" {
			return "release", nil, errors.Join(err, errors.New("release requires --lease-id"))
		}
		response, err := service.Release(ctx, v2.ReleaseRequest{RequestID: *requestID, LeaseID: *leaseID})
		return "release", response, err
	default:
		return args[0], nil, fmt.Errorf("unknown workspace operation %q", args[0])
	}
}

func invalidV2Response(requestID, operation string, err error) v2.Response {
	return v2.Response{SchemaVersion: v2.SchemaVersion, RequestID: requestID, OK: false, Error: &v2.Error{Code: "invalid-request", Operation: operation, Message: err.Error()}}
}

func readRequestData(args []string, stdin io.Reader) ([]byte, error) {
	flags := newFlagSet("workspace acquire")
	path := flags.String("request", "-", "request JSON path or stdin")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return nil, errors.Join(err, errors.New("acquire accepts only --request"))
	}
	if *path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(*path)
}
func decodeBytes(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
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
