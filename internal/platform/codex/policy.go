package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Codex exec has no supported switch that ignores managed layers. Query the
// same CLI's configuration loader without creating a thread or sending corpus.
// Its RPC sees platform policy paths, forced macOS preferences and cloud policy.
func checkManagedConfig(ctx context.Context, bin, dir string, env []string) error {
	cmd := commandContext(ctx, bin, append(isolationArgs(), "app-server", "--listen", "stdio://")...)
	cmd.Dir, cmd.Env = dir, env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer stdout.Close()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("Codex policy preflight: %w", err)
	}
	// Closing both pipes unblocks the scanner even if a CLI wrapper leaves a
	// child holding its stdout open after CommandContext kills the wrapper.
	stop := context.AfterFunc(ctx, func() { _ = stdin.Close(); _ = stdout.Close() })
	defer stop()
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	in := json.NewEncoder(stdin)
	out := bufio.NewScanner(stdout)
	out.Buffer(make([]byte, 4096), 4<<20)
	call := func(id int, method string, params any, dst any) error {
		if err := in.Encode(rpcRequest{id, method, params}); err != nil {
			return fmt.Errorf("Codex policy preflight %s: %w", method, err)
		}
		for out.Scan() {
			var reply struct {
				ID     int
				Result json.RawMessage
			}
			if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
				return fmt.Errorf("Codex policy preflight %s returned invalid JSON", method)
			}
			if reply.ID != id {
				continue // notification
			}
			if len(reply.Result) == 0 || bytes.Equal(reply.Result, []byte("null")) || json.Unmarshal(reply.Result, dst) != nil {
				return fmt.Errorf("Codex policy preflight %s did not return a usable result", method)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Codex policy preflight %s ended before verification", method)
	}
	var initialized map[string]any
	if err := call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "witness", "version": "1"}}, &initialized); err != nil {
		return err
	}
	if err := in.Encode(rpcRequest{Method: "initialized"}); err != nil {
		return fmt.Errorf("Codex policy preflight initialization: %w", err)
	}
	var layers struct {
		Layers []struct {
			Name   struct{ Type string }
			Config map[string]json.RawMessage
		}
	}
	if err := call(2, "config/read", map[string]any{"includeLayers": true, "cwd": dir}, &layers); err != nil {
		return err
	}
	if layers.Layers == nil {
		return fmt.Errorf("Codex policy preflight did not report config layers")
	}
	for _, layer := range layers.Layers {
		switch layer.Name.Type {
		case "packagedDefaults", "sessionFlags":
			continue
		case "system", "user":
			// Codex includes an empty placeholder for missing config files.
			if layer.Config != nil && len(layer.Config) == 0 {
				continue
			}
		}
		return fmt.Errorf("Codex runner cannot verify tool isolation with a %s config layer", layer.Name.Type)
	}
	var requirements struct{ Requirements json.RawMessage }
	if err := call(3, "configRequirements/read", nil, &requirements); err != nil {
		return err
	}
	if !bytes.Equal(requirements.Requirements, []byte("null")) {
		return fmt.Errorf("Codex runner cannot verify tool isolation with managed requirements")
	}

	return nil
}

type rpcRequest struct {
	ID     int    `json:"id,omitempty"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}
