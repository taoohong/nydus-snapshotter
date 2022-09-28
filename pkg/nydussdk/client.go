/*
 * Copyright (c) 2020. Ant Group. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package nydussdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/pkg/errors"

	"github.com/containerd/containerd/log"
	"github.com/containerd/nydus-snapshotter/config"
	"github.com/containerd/nydus-snapshotter/pkg/nydussdk/model"
	"github.com/containerd/nydus-snapshotter/pkg/utils/retry"
)

const (
	endpointDaemonInfo = "/api/v1/daemon"
	endpointMount      = "/api/v1/mount"
	endpointMetrics    = "/api/v1/metrics"

	defaultHTTPClientTimeout = 30 * time.Second
	contentType              = "application/json"
)

type Interface interface {
	CheckStatus() (*model.DaemonInfo, error)
	SharedMount(sharedMountPoint, bootstrap, daemonConfig string) error
	FscacheBindBlob(daemonConfig string) error
	FscacheUnbindBlob(daemonConfig string) error
	Umount(sharedMountPoint string) error
	GetFsMetric(sharedDaemon bool, sid string) (*model.FsMetric, error)
}

type NydusdClient struct {
	httpClient *http.Client
}

func NewNydusClient(sock string) (Interface, error) {
	transport, err := buildTransport(sock)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build transport for nydus client")
	}
	return &NydusdClient{
		httpClient: &http.Client{
			Timeout:   defaultHTTPClientTimeout,
			Transport: transport,
		},
	}, nil
}

func (c *NydusdClient) CheckStatus() (*model.DaemonInfo, error) {
	addr := fmt.Sprintf("http://unix%s", endpointDaemonInfo)
	resp, err := c.httpClient.Get(addr)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to do HTTP GET from %s", addr)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read status response")
	}
	var info model.DaemonInfo
	if err = json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func (c *NydusdClient) Umount(sharedMountPoint string) error {
	requestURL := fmt.Sprintf("http://unix%s?mountpoint=%s", endpointMount, sharedMountPoint)
	req, err := http.NewRequest(http.MethodDelete, requestURL, nil)
	if err != nil {
		return errors.Wrap(err, "failed to create umount request")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "failed to do HTTP DELETE to %s", requestURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return handleMountError(resp)
}

func (c *NydusdClient) GetFsMetric(sharedDaemon bool, sid string) (*model.FsMetric, error) {
	var getStatURL string

	if sharedDaemon {
		getStatURL = fmt.Sprintf("http://unix%s?id=/%s/fs", endpointMetrics, sid)
	} else {
		getStatURL = fmt.Sprintf("http://unix%s", endpointMetrics)
	}

	req, err := http.NewRequest(http.MethodGet, getStatURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create GetFsMetric request")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to do HTTP GET to %s", getStatURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, errors.New("got unexpected http status StatusNoContent")
	}

	var m model.FsMetric
	if err = json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, errors.Wrap(err, "failed to decode FsMetric")
	}
	return &m, nil
}

func (c *NydusdClient) SharedMount(sharedMountPoint, bootstrap, daemonConfig string) error {
	requestURL := fmt.Sprintf("http://unix%s?mountpoint=%s", endpointMount, sharedMountPoint)
	content, err := os.ReadFile(daemonConfig)
	if err != nil {
		return errors.Wrapf(err, "failed to get content of daemon config %s", daemonConfig)
	}
	body, err := json.Marshal(model.NewMountRequest(bootstrap, string(content)))
	if err != nil {
		return errors.Wrap(err, "failed to create mount request")
	}
	resp, err := c.httpClient.Post(requestURL, contentType, bytes.NewBuffer(body))
	if err != nil {
		return errors.Wrapf(err, "failed to do HTTP POST to %s", requestURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return handleMountError(resp)
}

func (c NydusdClient) FscacheBindBlob(daemonConfig string) error {
	log.L.Infof("requesting daemon to bind fscache blob with config %s", daemonConfig)

	body, err := os.ReadFile(daemonConfig)
	if err != nil {
		return errors.Wrapf(err, "failed to get content of daemon config %s", daemonConfig)
	}

	requestURL := "http://unix/api/v2/blobs"
	req, err := http.NewRequest(http.MethodPut, requestURL, bytes.NewBuffer(body))
	if err != nil {
		return errors.Wrapf(err, "failed to create request for url %s", requestURL)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "failed to do HTTP PUT to %s", requestURL)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	return handleMountError(resp)
}

func (c NydusdClient) FscacheUnbindBlob(daemonConfig string) error {
	body, err := os.ReadFile(daemonConfig)
	if err != nil {
		return errors.Wrapf(err, "failed to get content of daemon config %s", daemonConfig)
	}

	var cfg config.DaemonConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return errors.Wrap(err, "unmarshal fscache daemon config")
	}

	requestURL := fmt.Sprintf("http://unix/api/v2/blobs?domain_id=%s", cfg.DomainID)
	req, err := http.NewRequest(http.MethodDelete, requestURL, nil)
	if err != nil {
		return errors.Wrap(err, "failed to create fscache unbind blob request")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "failed to do HTTP DELETE to %s", requestURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return handleMountError(resp)
}

func WaitUntilSocketExisted(sock string) error {
	return retry.Do(func() (err error) {
		var st fs.FileInfo
		if st, err = os.Stat(sock); err != nil {
			return
		}

		if st.Mode()&os.ModeSocket == 0 {
			return errors.Errorf("file %s is not socket file", sock)
		}

		return nil
	},
		retry.Attempts(20), // totally wait for 2 seconds, should be enough
		retry.LastErrorOnly(true),
		retry.Delay(100*time.Millisecond))
}

// TODO: It's time-consuming. Make it execute only once by sync.Once
func buildTransport(sock string) (http.RoundTripper, error) {
	err := WaitUntilSocketExisted(sock)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		MaxIdleConns:          10,
		IdleConnTimeout:       10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := &net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 5 * time.Second,
			}
			return dialer.DialContext(ctx, "unix", sock)
		},
	}, nil
}

func handleMountError(resp *http.Response) error {
	var r io.Reader = resp.Body
	b, err := io.ReadAll(r)
	if err != nil {
		return errors.Wrap(err, "failed to read from reader")
	}
	var errMessage model.ErrorMessage
	if err = json.Unmarshal(b, &errMessage); err != nil {
		return errors.Wrap(err, "failed unmarshal ErrorMessage")
	}
	return fmt.Errorf("http response: %d, error code: %s, error message: %s", resp.StatusCode, errMessage.Code, errMessage.Message)
}
