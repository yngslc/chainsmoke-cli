// Tor control-protocol client.
package net

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type controlClient struct {
	conn net.Conn
	rw   *bufio.Reader
}

func dialControl(addr string, timeout time.Duration) (*controlClient, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("tor control port %s unreachable (enable ControlPort): %w", addr, err)
	}
	_ = c.SetDeadline(time.Now().Add(timeout))
	return &controlClient{conn: c, rw: bufio.NewReader(c)}, nil
}

func (c *controlClient) Close() error { return c.conn.Close() }

func (c *controlClient) exec(cmd string) ([]string, error) {
	if _, err := fmt.Fprintf(c.conn, "%s\r\n", cmd); err != nil {
		return nil, fmt.Errorf("tor control: write %q: %w", cmd, err)
	}
	return c.readReply()
}

func (c *controlClient) readReply() ([]string, error) {
	var lines []string
	for {
		raw, err := c.rw.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("tor control: read reply: %w", err)
		}
		line := strings.TrimRight(raw, "\r\n")
		if len(line) < 4 {
			continue
		}
		code, convErr := strconv.Atoi(line[:3])
		if convErr != nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "250 "), strings.HasPrefix(line, "251 "):
			if body := line[4:]; body != "OK" {
				lines = append(lines, body)
			}
			return lines, nil
		case strings.HasPrefix(line, "250-"), strings.HasPrefix(line, "251-"):
			lines = append(lines, line[4:])
		case strings.HasPrefix(line, "250+"):
			lines = append(lines, line[4:])
			block, err := c.readDataBlock()
			if err != nil {
				return nil, err
			}
			lines = append(lines, block...)
		case code >= 400 && code < 600:
			return nil, fmt.Errorf("tor control: %s", line)
		default:
		}
	}
}

func (c *controlClient) readDataBlock() ([]string, error) {
	var out []string
	for {
		raw, err := c.rw.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("tor control: read data block: %w", err)
		}
		line := strings.TrimRight(raw, "\r\n")
		if line == "." {
			return out, nil
		}
		out = append(out, strings.TrimPrefix(line, "."))
	}
}

func (c *controlClient) Authenticate() error {
	lines, err := c.exec("PROTOCOLINFO 1")
	if err != nil {
		return err
	}

	var methods, cookiePath string
	for _, ln := range lines {
		if i := strings.Index(ln, "METHODS="); i >= 0 && strings.HasPrefix(ln, "AUTH") {
			if f := strings.Fields(ln[i+len("METHODS="):]); len(f) > 0 {
				methods = f[0]
			}
		}
		if i := strings.Index(ln, `COOKIEFILE="`); i >= 0 {
			rest := ln[i+len(`COOKIEFILE="`):]
			if j := strings.Index(rest, `"`); j >= 0 {
				cookiePath = filepath.Clean(unescapeCString(rest[:j]))
			}
		}
	}

	switch {
	case cookiePath != "":
		data, err := os.ReadFile(cookiePath)
		if err != nil {
			return fmt.Errorf("tor control: cannot read cookie %s (add your user to tor's group?): %w", cookiePath, err)
		}
		_, err = c.exec("AUTHENTICATE " + strings.ToUpper(hex.EncodeToString(data)))
		return err
	case strings.Contains(methods, "NULL"):
		_, err := c.exec("AUTHENTICATE")
		return err
	case strings.Contains(methods, "HASHEDPASSWORD"):
		return fmt.Errorf("tor control: this tor requires HASHEDPASSWORD auth; " +
			"chainsmoke needs CookieAuthentication 1, or let it spawn its own tor")
	default:
		return fmt.Errorf("tor control: no usable auth method (advertised: %q)", methods)
	}
}

func unescapeCString(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(s)
}

func (c *controlClient) GetInfo(key string) (string, error) {
	lines, err := c.exec("GETINFO " + key)
	if err != nil {
		return "", err
	}
	for _, ln := range lines {
		if v, ok := strings.CutPrefix(ln, key+"="); ok {
			return v, nil
		}
	}
	if len(lines) > 0 {
		return lines[0], nil
	}
	return "", fmt.Errorf("tor control: no value for %s", key)
}

func (c *controlClient) NewNym() error {
	_, err := c.exec("SIGNAL NEWNYM")
	return err
}
