package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// powerOn is the optional "Power on" button on the offline page. What it does is up to the
// configuration, since every machine is switched on differently: either an HTTP request (a BMC's
// Redfish API such as HPE iLO or Dell iDRAC, a smart plug, a Home Assistant webhook, a
// Wake-on-LAN relay) or a shell command (wakeonlan, ipmitool, ssh to another box). The HTTP way
// works in the published image, which has no shell; a command needs a binary install or an image
// of your own with the tools in it.
type powerOn struct {
	url, method, body, contentType string
	authFile                       string // "user:password" for Basic auth, read at each use
	insecure                       bool   // skip TLS verification, BMCs mostly have self-signed certs
	command                        string // run with sh -c instead of an HTTP request

	mu   sync.Mutex
	sent time.Time // the last successful power-on
}

// powerCooldown: after a power-on, the button stays disabled this long. A cold start takes
// minutes, and a second press during the boot could switch the machine off again, e.g. with a
// Redfish "PushPowerButton".
const powerCooldown = 5 * time.Minute

const powerTimeout = 30 * time.Second

func loadPowerOn() (*powerOn, error) {
	p := &powerOn{
		url:         env("PODSWITCH_POWER_ON_URL", ""),
		method:      env("PODSWITCH_POWER_ON_METHOD", "POST"),
		body:        env("PODSWITCH_POWER_ON_BODY", ""),
		contentType: env("PODSWITCH_POWER_ON_CONTENT_TYPE", "application/json"),
		authFile:    env("PODSWITCH_POWER_ON_AUTH", ""),
		insecure:    env("PODSWITCH_POWER_ON_INSECURE", "") == "true",
		command:     env("PODSWITCH_POWER_ON_CMD", ""),
	}
	switch {
	case p.url == "" && p.command == "":
		return nil, nil
	case p.url != "" && p.command != "":
		return nil, errors.New("set PODSWITCH_POWER_ON_URL or PODSWITCH_POWER_ON_CMD, not both")
	}
	return p, nil
}

// since returns when the last power-on was sent, if that is within the cooldown.
func (p *powerOn) since() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent, !p.sent.IsZero() && time.Since(p.sent) < powerCooldown
}

// run sends the power-on, unless one was sent within the cooldown.
func (p *powerOn) run() error {
	p.mu.Lock()
	defer p.mu.Unlock() // one at a time: a second click waits, then sees the cooldown
	if !p.sent.IsZero() && time.Since(p.sent) < powerCooldown {
		return errors.New("already sent at " + p.sent.Format("15:04:05"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), powerTimeout)
	defer cancel()
	var err error
	if p.command != "" {
		err = p.runCommand(ctx)
	} else {
		err = p.runRequest(ctx)
	}
	if err == nil {
		p.sent = time.Now()
	}
	return err
}

func (p *powerOn) runCommand(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "sh", "-c", p.command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("power-on command: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *powerOn) runRequest(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, p.method, p.url, strings.NewReader(p.body))
	if err != nil {
		return err
	}
	if p.body != "" {
		req.Header.Set("Content-Type", p.contentType)
	}
	if p.authFile != "" {
		b, err := os.ReadFile(p.authFile)
		if err != nil {
			return fmt.Errorf("power-on auth: %w", err)
		}
		user, pass, ok := strings.Cut(string(bytes.TrimSpace(b)), ":")
		if !ok {
			return fmt.Errorf("power-on auth: %s must hold user:password", p.authFile)
		}
		req.SetBasicAuth(user, pass)
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: p.insecure},
	}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("power-on: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("power-on: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
