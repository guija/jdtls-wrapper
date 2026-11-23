package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type jdtResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result"`
}

type stdResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Result  []stdResult `json:"result"`
}

type jdtResult struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type stdResult struct {
	TargetURI            string `json:"targetUri"`
	TargetRange          Range  `json:"targetRange"`
	TargetSelectionRange Range  `json:"targetSelectionRange"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Position struct {
	Line      int64 `json:"line"`
	Character int64 `json:"character"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cmd := exec.Command("jdtls", os.Args[1:]...)

	serverStdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	serverStdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	serverStderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// Forward client -> server
	m := make(map[string]string)
	var mu sync.Mutex
	var serverMu sync.Mutex
	writeToServer := func(body []byte) error {
		serverMu.Lock()
		defer serverMu.Unlock()
		return writeLSPMessage(serverStdin, body)
	}
	go func() {
		defer func() {
			serverMu.Lock()
			defer serverMu.Unlock()
			serverStdin.Close()
		}()
		reader := bufio.NewReader(os.Stdin)
		for {
			body, err := readLSPMessage(reader)
			if err != nil {
				fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
				return
			}

			var msg map[string]any
			if err := json.Unmarshal(body, &msg); err != nil {
				fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
				return
			}

			if params, ok := msg["params"].(map[string]any); ok {
				if textDocument, ok := params["textDocument"].(map[string]any); ok {
					if uri, ok := textDocument["uri"].(string); ok {
						mu.Lock()
						jdtURI, ok := m[uri]
						mu.Unlock()
						if ok {
							textDocument["uri"] = jdtURI

							body, err = json.Marshal(msg)
							if err != nil {
								fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
								return
							}
						}
					}
				}
			}

			if err := writeToServer(body); err != nil {
				fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
				return
			}
		}
	}()

	// Forward server -> client
	pending := make(map[int]func(*jdtResponse))
	go func() {
		reader := bufio.NewReader(serverStdout)
		for {
			body, jdtResp, err := readJDTResponse(reader)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
				}
				return
			}

			fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", string(body))

			id, ok := getID(jdtResp.ID)
			if !ok {
				if err := writeLSPMessage(os.Stdout, body); err != nil {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					return
				}
				continue
			}

			if cb, ok := pending[id]; ok {
				cb(jdtResp)
				delete(pending, id)
				continue
			}

			if len(jdtResp.Result) == 0 {
				if err := writeLSPMessage(os.Stdout, body); err != nil {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					return
				}
				continue
			}

			var definitionResult []jdtResult
			if err := json.Unmarshal(jdtResp.Result, &definitionResult); err != nil {
				if err := writeLSPMessage(os.Stdout, body); err != nil {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					return
				}
				continue
			}

			if len(definitionResult) == 0 {
				if err := writeLSPMessage(os.Stdout, body); err != nil {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					return
				}
				continue
			}

			uri, err := url.Parse(definitionResult[0].URI)
			if err != nil {
				fmt.Fprintln(os.Stderr, "[jdtls-wrapper]: invalid uri", definitionResult[0].URI)
			}
			fmt.Fprintf(os.Stderr, "path: %s\n", uri.Path)

			if uri.Scheme == "jdt" {
				newID := id + 1
				classReq := map[string]any{
					"id":      newID,
					"jsonrpc": "2.0",
					"method":  "java/classFileContents",
					"params": map[string]any{
						"uri": definitionResult[0].URI,
					},
				}
				data, err := json.Marshal(classReq)
				if err != nil {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
				}

				pending[newID] = func(resp *jdtResponse) {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", string(resp.Result))
					result, err := strconv.Unquote(string(resp.Result))
					if err != nil {
						fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					}

					result = strings.ReplaceAll(result, `\n`, "\n")
					result = strings.ReplaceAll(result, `\t`, "\t")
					tmpFileName := "/tmp" + strings.TrimSuffix(uri.Path, ".class") + ".java"
					targetURI := "file://" + tmpFileName
					mu.Lock()
					m[targetURI] = uri.String()
					mu.Unlock()
					if err := os.MkdirAll(filepath.Dir(tmpFileName), 0755); err != nil {
						fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					}
					if err := os.WriteFile(tmpFileName, []byte(result), 0400); err != nil {
						fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					}
					targetRange := Range{
						Start: Position{
							Line:      definitionResult[0].Range.Start.Line,
							Character: definitionResult[0].Range.Start.Character,
						},
						End: Position{
							Line:      definitionResult[0].Range.End.Line,
							Character: definitionResult[0].Range.End.Character,
						},
					}
					stdResp := &stdResponse{
						Jsonrpc: "2.0",
						ID:      int64(id),
						Result: []stdResult{
							{
								TargetURI:            targetURI,
								TargetRange:          targetRange,
								TargetSelectionRange: targetRange,
							},
						},
					}

					data, err := json.Marshal(stdResp)
					if err == nil {
						if err := writeLSPMessage(os.Stdout, data); err != nil {
							fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
						}
					}
				}

				if err := writeToServer(data); err != nil {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					return
				}
			} else {
				if err := writeLSPMessage(os.Stdout, body); err != nil {
					fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", err.Error())
					return
				}
			}
		}
	}()

	scanner := bufio.NewScanner(serverStderr)
	go func() {
		for scanner.Scan() {
			fmt.Fprintln(os.Stderr, "[jdtls-wrapper]", scanner.Text())
		}
	}()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("jdtls exited: %w", err)
	}

	return nil
}

func readLSPMessage(r *bufio.Reader) (body []byte, err error) {
	var contentLength int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			break
		}

		if strings.HasPrefix(line, "Content-Length:") {
			lengthStr := strings.TrimSpace(line[len("Content-Length:"):])
			length, err := strconv.Atoi(lengthStr)
			if err != nil {
				return nil, err
			}
			contentLength = length
		}
	}

	buf := make([]byte, contentLength)
	_, err = io.ReadFull(r, buf)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

func readJDTResponse(r *bufio.Reader) ([]byte, *jdtResponse, error) {
	body, err := readLSPMessage(r)
	if err != nil {
		return nil, nil, err
	}

	var response *jdtResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return body, nil, err
	}
	if response == nil {
		return body, nil, errors.New("null LSP response")
	}

	return body, response, nil
}

func writeLSPMessage(w io.Writer, body []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}

	_, err := w.Write(body)
	return err
}

func getID(id interface{}) (int, bool) {
	switch v := id.(type) {
	case float64:
		return int(v), true
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
		return 0, false
	default:
		return 0, false
	}
}
