/*
 * MIT License
 *
 * Copyright (c) 2025 Jianhui Zhao <zhaojh329@gmail.com>
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	rttyCmdRunningLimit  = 5
	rttyCmdExecTimeout   = 30 * time.Second
	rttyCmdMaxOutputSize = 1024 * 1024
)

const (
	rttyCmdErrNone = iota
	rttyCmdErrPermit
	rttyCmdErrNotFound
	rttyCmdErrNoMem
	rttyCmdErrSysErr
	rttyCmdErrRespTooBig
)

var rttyCmdSemaphore = make(chan struct{}, rttyCmdRunningLimit)

func handleCmdMsg(cli *RttyClient, data []byte) error {
	username, cmdName, token, params, err := parseCmdMsg(data)
	if err != nil {
		log.Error().Err(err).Msg("invalid command message format")
		return nil
	}

	log.Debug().Msgf("command: %s, username: %s, token: %s, params: %v", cmdName, username, token, params)

	u, err := user.Lookup(username)
	if err != nil {
		cmdErrReply(cli, token, rttyCmdErrPermit)
		return nil
	}

	cmdPath, err := exec.LookPath(cmdName)
	if cmdPath == "" {
		log.Error().Err(err).Msgf("command not found: %s", cmdName)
		cmdErrReply(cli, token, rttyCmdErrNotFound)
		return nil
	}

	select {
	case rttyCmdSemaphore <- struct{}{}:
		go executeCommand(cli, u, cmdPath, params, token)
	default:
		log.Warn().Msgf("command limit reached: %d", rttyCmdRunningLimit)
		cmdErrReply(cli, token, rttyCmdErrNoMem)
	}

	return nil
}

func executeCommand(cli *RttyClient, u *user.User, cmdPath string, params []string, token string) {
	defer func() {
		<-rttyCmdSemaphore
	}()

	log.Debug().Msgf("starting command execution: %s, token: %s", cmdPath, token)

	ctx, cancel := context.WithTimeout(context.Background(), rttyCmdExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, cmdPath, params...)

	setSysProcAttr(cmd, u)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	err := cmd.Run()

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Error().Msgf("command timeout: %s, token: %s", cmdPath, token)
			cmdErrReply(cli, token, rttyCmdErrSysErr)
			return
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			log.Error().Err(err).Msgf("command execution failed: %s, token: %s", cmdPath, token)
			cmdErrReply(cli, token, rttyCmdErrSysErr)
			return
		}
	}

	stdoutBytes := stdout.Bytes()
	stderrBytes := stderr.Bytes()

	if len(stdoutBytes)+len(stderrBytes) > rttyCmdMaxOutputSize {
		log.Error().Msgf("command output too large: %s, token: %s", cmdPath, token)
		cmdErrReply(cli, token, rttyCmdErrRespTooBig)
		return
	}

	cmdReply(cli, token, exitCode, stdoutBytes, stderrBytes)
}

func parseCmdMsg(data []byte) (string, string, string, []string, error) {
	var parts []string

	for {
		i := bytes.Index(data, []byte{0})
		if i < 0 {
			return "", "", "", nil, fmt.Errorf("invalid command message format")
		}

		parts = append(parts, string(data[:i]))
		data = data[i+1:]

		if len(data) == 0 {
			return "", "", "", nil, fmt.Errorf("invalid command message format")
		}

		if len(parts) == 3 {
			break
		}
	}

	if len(data) < 1 {
		return "", "", "", nil, fmt.Errorf("invalid command message format")
	}

	var params []string

	nparams := data[0]

	if nparams > 0 {
		data = bytes.TrimSuffix(data[1:], []byte{0})
		params = strings.Split(string(data), "\x00")

		if len(params) != int(nparams) {
			return "", "", "", nil, fmt.Errorf("invalid command message format: expected %d params, got %d", nparams, len(params))
		}
	}

	return parts[0], parts[1], parts[2], params, nil
}

func cmdErrReply(cli *RttyClient, token string, err int) {
	msg := fmt.Sprintf(`{"token":"%s","attrs":{"err":%d,"msg":"%s"}}`, token, err, cmderr2str(err))
	cli.SendMsg(MsgTypeCmd, msg)
}

func cmderr2str(err int) string {
	switch err {
	case rttyCmdErrPermit:
		return "operation not permitted"
	case rttyCmdErrNotFound:
		return "not found"
	case rttyCmdErrNoMem:
		return "no mem"
	case rttyCmdErrSysErr:
		return "sys error"
	case rttyCmdErrRespTooBig:
		return "stdout+stderr is too big"
	default:
		return ""
	}
}

func cmdReply(cli *RttyClient, token string, code int, stdout []byte, stderr []byte) {
	stdoutB64 := base64.StdEncoding.EncodeToString(stdout)
	stderrB64 := base64.StdEncoding.EncodeToString(stderr)
	msg := fmt.Sprintf(`{"token":"%s","attrs":{"code":%d,"stdout":"%s","stderr":"%s"}}`, token, code, stdoutB64, stderrB64)
	cli.SendMsg(MsgTypeCmd, msg)
}
