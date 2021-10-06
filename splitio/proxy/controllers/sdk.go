package controllers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/splitio/go-split-commons/v4/dtos"
	"github.com/splitio/go-split-commons/v4/service"
	"github.com/splitio/go-toolkit/v5/logging"

	tmw "github.com/splitio/split-synchronizer/v4/splitio/proxy/controllers/middleware"
	"github.com/splitio/split-synchronizer/v4/splitio/proxy/storage"
)

// SdkServerController bundles all request handler for sdk-server apis
type SdkServerController struct {
	logger              logging.LoggerInterface
	fetcher             service.SplitFetcher
	proxySplitStorage   storage.ProxySplitStorage
	proxySegmentStorage storage.ProxySegmentStorage
	telemetry           storage.ProxyEndpointTelemetry
}

// NewSdkServerController instantiates a new sdk server controller
func NewSdkServerController(
	logger logging.LoggerInterface,
	fetcher service.SplitFetcher,
	proxySplitStorage storage.ProxySplitStorage,
	proxySegmentStorage storage.ProxySegmentStorage,
	telemetry storage.ProxyEndpointTelemetry,
) *SdkServerController {
	return &SdkServerController{
		logger:              logger,
		fetcher:             fetcher,
		proxySplitStorage:   proxySplitStorage,
		proxySegmentStorage: proxySegmentStorage,
		telemetry:           telemetry,
	}
}

// Register mounts the sdk-server endpoints onto the supplied router
func (c *SdkServerController) Register(router gin.IRouter) {
	router.GET("/splitChanges", c.SplitChanges)
	router.GET("/segmentChanges/:name", c.SegmentChanges)
	router.GET("/mySegments/:key", c.MySegments)
}

// SplitChanges Returns a diff containing changes in splits from a certain point in time until now.
func (c *SdkServerController) SplitChanges(ctx *gin.Context) {
	ctx.Set(tmw.EndpointKey, storage.SplitChangesEndpoint)
	c.logger.Debug(fmt.Sprintf("Headers: %v", ctx.Request.Header))
	since, err := strconv.ParseInt(ctx.DefaultQuery("since", "-1"), 10, 64)
	if err != nil {
		since = -1
	}
	c.logger.Debug(fmt.Sprintf("SDK Fetches Splits Since: %d", since))

	splits, err := c.fetchSplitChangesSince(since)
	if err != nil {
		c.telemetry.IncrEndpointStatus(storage.SplitChangesEndpoint, http.StatusInternalServerError)
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.telemetry.IncrEndpointStatus(storage.SplitChangesEndpoint, http.StatusOK)
	ctx.JSON(http.StatusOK, splits)
}

// SegmentChanges Returns a diff containing changes in splits from a certain point in time until now.
func (c *SdkServerController) SegmentChanges(ctx *gin.Context) {
	ctx.Set(tmw.EndpointKey, storage.SegmentChangesEndpoint)
	c.logger.Debug(fmt.Sprintf("Headers: %v", ctx.Request.Header))
	since, err := strconv.ParseInt(ctx.DefaultQuery("since", "-1"), 10, 64)
	if err != nil {
		since = -1
	}

	segmentName := ctx.Param("name")
	c.logger.Debug(fmt.Sprintf("SDK Fetches Segment: %s Since: %d", segmentName, since))
	payload, err := c.proxySegmentStorage.ChangesSince(segmentName, since)
	if err != nil {
		c.telemetry.IncrEndpointStatus(storage.SegmentChangesEndpoint, http.StatusNotFound)
		ctx.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, payload)
	c.telemetry.IncrEndpointStatus(storage.SegmentChangesEndpoint, http.StatusOK)
}

// MySegments Returns a diff containing changes in splits from a certain point in time until now.
func (c *SdkServerController) MySegments(ctx *gin.Context) {
	ctx.Set(tmw.EndpointKey, storage.MySegmentsEndpoint)
	c.logger.Debug(fmt.Sprintf("Headers: %v", ctx.Request.Header))
	key := ctx.Param("key")
	segmentList, err := c.proxySegmentStorage.SegmentsFor(key)
	if err != nil {
		c.logger.Error(fmt.Sprintf("error fetching segments for user '%s': %s", key, err.Error()))
		c.telemetry.IncrEndpointStatus(storage.MySegmentsEndpoint, http.StatusInternalServerError)
		ctx.JSON(http.StatusInternalServerError, gin.H{})
	}

	mySegments := make([]dtos.MySegmentDTO, 0, len(segmentList))
	for _, segmentName := range segmentList {
		mySegments = append(mySegments, dtos.MySegmentDTO{Name: segmentName})
	}

	ctx.JSON(http.StatusOK, gin.H{"mySegments": mySegments})
	c.telemetry.IncrEndpointStatus(storage.MySegmentsEndpoint, http.StatusOK)
}

func (c *SdkServerController) fetchSplitChangesSince(since int64) (*dtos.SplitChangesDTO, error) {
	splits, err := c.proxySplitStorage.ChangesSince(since)
	if err == nil {
		return splits, nil
	}
	if !errors.Is(err, storage.ErrSummaryNotCached) {
		return nil, fmt.Errorf("unexpected error fetching split changes from storage: %w", err)
	}

	splits, err = c.fetcher.Fetch(since, true)
	if err == nil {
		c.proxySplitStorage.RegisterOlderCn(splits)
		return splits, nil
	}
	return nil, fmt.Errorf("unexpected error fetching split changes from storage: %w", err)
}
