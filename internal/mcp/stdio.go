package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func RunStdio(ctx context.Context, srv *Server) error {
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			return err
		}
		result, err := srv.dispatch(ctx, req)
		resp := Response{JSONRPC: "2.0", ID: req.ID}
		if err != nil {
			resp.Error = &ResponseError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = result
		}
		data, _ := json.Marshal(resp)
		if _, err := writer.Write(append(data, '\n')); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("stdio scan error: %w", err)
	}
	return nil
}
