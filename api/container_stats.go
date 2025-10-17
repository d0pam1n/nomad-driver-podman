// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var ContainerNotFound = errors.New("No such Container")
var ContainerWrongState = errors.New("Container has wrong state")

// ContainerStats data takes a name or ID of a container returns stats data
func (c *API) ContainerStats(ctx context.Context, name string) (Stats, error) {

	var stats Stats
	res, err := c.Get(ctx, fmt.Sprintf("/v1.0.0/libpod/containers/%s/stats?stream=false", name))
	if err != nil {
		return stats, err
	}

	defer ignoreClose(res.Body)

	if res.StatusCode == http.StatusNotFound {
		return stats, ContainerNotFound
	}

	if res.StatusCode == http.StatusConflict {
		return stats, ContainerWrongState
	}
	if res.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("cannot get stats of container, status code: %d", res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return stats, err
	}

	// Since podman 4.1.1, an empty 200 response is returned for stopped containers.
	if len(body) == 0 {
		return stats, ContainerNotFound
	}

	// Since podman 4.6.0, a 200 response with `container is stopped` is returned for stopped containers.
	var errResponse Error
	if _ = json.Unmarshal(body, &errResponse); errResponse.Cause == "container is stopped" {
		return stats, ContainerNotFound
	}

	err = json.Unmarshal(body, &stats)
	if err != nil {
		return stats, err
	}

	return stats, nil
}

type StatsBroadcaster interface {
	Subscribe() (<-chan []*ContainerStats, <-chan error)
	CancelSubscription(<-chan []*ContainerStats, <-chan error)
}

type containerStatsBroadcaster struct {
	statsChan           <-chan []*ContainerStats
	errStatsChan        <-chan error
	statsListeners      []chan []*ContainerStats
	errListeners        []chan error
	addStatsListener    chan chan []*ContainerStats
	addErrListener      chan chan error
	removeStatsListener chan (<-chan []*ContainerStats)
	removeErrListener   chan (<-chan error)
}

func NewContainerStatsBroadcaster(ctx context.Context, statsChan <-chan []*ContainerStats, errStatsChan <-chan error) *containerStatsBroadcaster {
	broadcaster := &containerStatsBroadcaster{
		statsChan:           statsChan,
		errStatsChan:        errStatsChan,
		statsListeners:      make([]chan []*ContainerStats, 0),
		errListeners:        make([]chan error, 0),
		addStatsListener:    make(chan chan []*ContainerStats),
		addErrListener:      make(chan chan error),
		removeStatsListener: make(chan (<-chan []*ContainerStats)),
		removeErrListener:   make(chan (<-chan error)),
	}

	go broadcaster.serve(ctx)
	return broadcaster
}

func (c *containerStatsBroadcaster) Subscribe() (<-chan []*ContainerStats, <-chan error) {
	newStatsListener := make(chan []*ContainerStats)
	newErrListener := make(chan error)
	c.addStatsListener <- newStatsListener
	c.addErrListener <- newErrListener
	return newStatsListener, newErrListener
}

func (c *containerStatsBroadcaster) CancelSubscription(ch <-chan []*ContainerStats, errCh <-chan error) {
	c.removeStatsListener <- ch
	c.removeErrListener <- errCh
}

func (c *containerStatsBroadcaster) serve(ctx context.Context) {
	defer func() {
		for _, statsListeners := range c.statsListeners {
			if statsListeners != nil {
				close(statsListeners)
			}
		}
		for _, errListeners := range c.errListeners {
			if errListeners != nil {
				close(errListeners)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case newStatsListener := <-c.addStatsListener:
			c.statsListeners = append(c.statsListeners, newStatsListener)
		case newErrListener := <-c.addErrListener:
			c.errListeners = append(c.errListeners, newErrListener)
		case removeStatsListener := <-c.removeStatsListener:
			for i, listener := range c.statsListeners {
				if listener == removeStatsListener {
					c.statsListeners = append(c.statsListeners[:i], c.statsListeners[i+1:]...)
					close(listener)
					break
				}
			}
		case removeErrListener := <-c.removeErrListener:
			for i, listener := range c.errListeners {
				if listener == removeErrListener {
					c.errListeners = append(c.errListeners[:i], c.errListeners[i+1:]...)
					close(listener)
					break
				}
			}
		case stats := <-c.statsChan:
			for _, listener := range c.statsListeners {
				select {
				case listener <- stats:
				default:
				}
			}
		case err := <-c.errStatsChan:
			for _, listener := range c.errListeners {
				select {
				case listener <- err:
				default:
				}
			}
		}
	}
}

func (c *API) ContainerStatsStream(ctx context.Context) {
	if c.isContainerStatsCollectorRunning {
		c.logger.Debug("Container stats collector is already running, skipping...")
		return
	}
	c.isContainerStatsCollectorRunning = true

	statsChan := make(chan []*ContainerStats)
	errChan := make(chan error, 1)

	// Wrap the stats in a struct to match the API response
	// This is needed to decode the JSON response from Podman
	// since it returns an object with "error" and "stats" fields
	type AllContainerStats struct {
		Error Error             `json:"error"`
		Stats []*ContainerStats `json:"stats"`
	}

	go func() {
		timer := time.NewTicker(time.Duration(1) * time.Second)
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				close(statsChan)
				close(errChan)
				return
			case <-timer.C:
				timer.Reset(c.containerStatsCollectInterval)
			}

			res, err := c.Get(ctx, "/v3.0.0/libpod/containers/stats?stream=false")
			if err != nil {
				c.logger.Error("Error getting container stats", "error", err)
				errChan <- err
				continue
			}
			dec := json.NewDecoder(res.Body)
			var stats AllContainerStats
			if err := dec.Decode(&stats); err != nil {
				c.logger.Error("Error decoding container stats", "error", err)
				errChan <- err
				continue
			}
			select {
			case statsChan <- stats.Stats:
			default:
			}
		}
	}()

	c.containerStatsBroadcaster = NewContainerStatsBroadcaster(ctx, statsChan, errChan)
}
