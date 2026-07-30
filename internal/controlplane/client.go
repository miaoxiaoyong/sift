package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miaoxiaoyong/sift/internal/config"
	"github.com/miaoxiaoyong/sift/internal/schema"
)

// OperatorRequest sends one operator RPC. It deliberately has no database
// fallback: all operator commands are daemon requests.
func OperatorRequest(home config.Home, method string, params map[string]any) (Response, error) {
	token, err := readOperatorToken(filepath.Join(home.Path, "operator.token"))
	if err != nil {
		return Response{}, err
	}
	id, err := randomID()
	if err != nil {
		return Response{}, err
	}
	c, err := net.DialTimeout("unix", socketPath(home.Path, "siftd.sock"), deadline)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(deadline))
	req := Request{ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, ClientVersion: Version, RequestID: id, Method: method, Auth: Auth{Kind: "operator", Token: token}, Params: params}
	if err := writeFrame(c, req); err != nil {
		return Response{}, err
	}
	body, err := readFrame(c)
	if err != nil {
		return Response{}, err
	}
	var response Response
	if err := schema.Decode(body, &response, schema.Closed); err != nil {
		return Response{}, fmt.Errorf("invalid daemon response: %w", err)
	}
	return response, nil
}

// RunReportRequest sends one report.submit RPC over run.sock using the run token
// read from SIFT_RUN_DIR/control.json. It never connects to siftd.sock, reads
// operator.token, or falls back to offline writes (report.md §1, §2).
func RunReportRequest(home config.Home, auth Auth, params map[string]any) (Response, error) {
	id, err := randomID()
	if err != nil {
		return Response{}, err
	}
	c, err := net.DialTimeout("unix", socketPath(home.Path, "run.sock"), deadline)
	if err != nil {
		return Response{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(deadline))
	req := Request{ProtocolMajor: ProtocolMajor, ProtocolMinor: ProtocolMinor, ClientVersion: Version, RequestID: id, Method: "report.submit", Auth: auth, Params: params}
	if err := writeFrame(c, req); err != nil {
		return Response{}, err
	}
	body, err := readFrame(c)
	if err != nil {
		return Response{}, err
	}
	var response Response
	if err := schema.Decode(body, &response, schema.Closed); err != nil {
		return Response{}, fmt.Errorf("invalid daemon response: %w", err)
	}
	return response, nil
}
func readOperatorToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
		return "", errf("operator token has unsafe type or permissions")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSuffix(string(b), "\n")
	if string(b) != token+"\n" || !validToken(token) {
		return "", errf("operator token is invalid")
	}
	return token, nil
}
func randomID() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
