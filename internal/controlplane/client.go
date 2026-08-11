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
	"github.com/miaoxiaoyong/sift/internal/version"
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
	return readResponse(c, id)
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
	return readResponse(c, id)
}

// readResponse verifies every response envelope before exposing either result
// or error to an RPC caller. A canonical handshake rejection is itself a
// validated observation: it is the sole exception to compatible protocol or
// binary majors, so doctor can report the daemon mismatch without consuming a
// response from an incompatible server.
func readResponse(c net.Conn, requestID string) (Response, error) {
	body, err := readFrame(c)
	if err != nil {
		return Response{}, err
	}
	var response Response
	if err := schema.Decode(body, &response, schema.Closed); err != nil {
		return Response{}, fmt.Errorf("invalid daemon response: %w", err)
	}
	if err := validateResponseEnvelope(response, requestID); err != nil {
		return Response{}, fmt.Errorf("invalid daemon response: %w", err)
	}
	response.envelopeValidated = true
	return response, nil
}

func validateResponseEnvelope(response Response, requestID string) error {
	if response.RequestID != requestID {
		return errf("response request id does not match request")
	}
	if !version.IsValidSemver(response.ServerVersion) {
		return errf("response server version is not canonical SemVer")
	}
	if response.OK {
		if response.Result == nil || response.Error != nil {
			return errf("response ok/result/error combination is invalid")
		}
	} else if response.Error == nil || response.Result != nil || response.Error.Code == "" || response.Error.Message == "" || response.Error.Details == nil {
		return errf("response ok/result/error combination is invalid")
	}

	// A negative protocol_minor is not "older but compatible": V0 is a closed
	// contract, so it is treated as incompatible and is only consumable as the
	// canonical unsupported_protocol handshake rejection below.
	protocolCompatible := response.ProtocolMajor == ProtocolMajor && response.ProtocolMinor >= 0 && response.ProtocolMinor <= ProtocolMinor
	if !protocolCompatible {
		if response.OK || response.Error.Code != "unsupported_protocol" {
			return errf("response protocol is incompatible")
		}
		return nil
	}
	if majorVersion(response.ServerVersion) != majorVersion(Version) {
		if response.OK || response.Error.Code != "unsupported_binary" {
			return errf("response server binary major is incompatible")
		}
		return nil
	}
	if !response.OK && (response.Error.Code == "unsupported_protocol" || response.Error.Code == "unsupported_binary") {
		return errf("response handshake error conflicts with compatible versions")
	}
	return nil
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
