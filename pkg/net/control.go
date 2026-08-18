// Tor control-protocol client (T2.4).
//
// Portions of the reply grammar and the PROTOCOLINFO cookie flow are derived
// from github.com/nao1215/tornago (MIT, Copyright (c) 2025 nao1215).
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

// controlClient speaks Tor's control protocol. Two things here are easy to get
// wrong and were wrong in the first implementation:
//
//  1. Auth. `AUTHENTICATE ""` only works on a control port with no
//     authentication. Every distro tor package ships `CookieAuthentication 1`,
//     so that version failed on any normal install with a misleading error.
//  2. The reply grammar. Tor answers with `250-` continuations, `250+` data
//     blocks terminated by a lone ".", and interleaves asynchronous `6xx`
//     events. Reading exactly one line per command desynchronizes the
//     connection as soon as any of that happens.
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

// readReply implements Tor's control reply grammar: a run of "250-"
// continuations (or a "250+" block terminated by a lone "."), closed by a
// single "250 " line. Codes >= 400 are errors. Anything else is an asynchronous
// event and must be SKIPPED, not mistaken for our answer.
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
			// 4xx/5xx are command failures. NOT >= 400: 6xx is the async event
			// range, and Tor emits those unsolicited at any moment — a bare
			// `code >= 400` turns a routine "650 CIRC ... EXTENDED" notice into
			// a spurious command error. (The reference implementation this was
			// ported from has that bug; TestReadReplyHandlesMultilineAndEvents
			// is what caught it.)
			return nil, fmt.Errorf("tor control: %s", line)
		default:
			// 1xx/2xx-continuation handled above, 6xx asynchronous event here —
			// not our reply, keep reading.
		}
	}
}

// readDataBlock consumes a "250+" payload up to the terminating lone dot.
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

// Authenticate negotiates whatever auth this tor was built with, instead of
// assuming there is none.
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

// unescapeCString undoes the C-style quoting Tor uses in PROTOCOLINFO paths.
// A Windows cookie path arrives as C:\\Users\\... and a naive parse leaves the
// doubled backslashes in place, so the file is never found.
func unescapeCString(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(s)
}

// GetInfo returns a single GETINFO value, e.g. status/bootstrap-phase.
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

// NewNym asks for a fresh circuit.
func (c *controlClient) NewNym() error {
	_, err := c.exec("SIGNAL NEWNYM")
	return err
}
