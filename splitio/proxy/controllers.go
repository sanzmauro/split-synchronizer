package proxy

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"

	"github.com/splitio/go-agent/log"
	"github.com/splitio/go-agent/splitio"
	"github.com/splitio/go-agent/splitio/api"
	"github.com/splitio/go-agent/splitio/proxy/controllers"
	"github.com/splitio/go-agent/splitio/stats"
	"github.com/splitio/go-agent/splitio/stats/counter"
	"github.com/splitio/go-agent/splitio/stats/latency"
	"github.com/splitio/go-agent/splitio/storage/boltdb"
	"github.com/splitio/go-agent/splitio/storage/boltdb/collections"
	"gopkg.in/gin-gonic/gin.v1"
)

var controllerLatencies = latency.NewLatency()
var controllerCounters = counter.NewCounter()

const latencyFetchSplitsFromDB = "goproxy.FetchSplitsFromBoltDB"
const latencyFetchSegmentFromDB = "goproxy.FetchSegmentFromBoltDB"
const latencyAddImpressionsInBuffer = "goproxy.AddImpressionsInBuffer"
const latencyPostSDKLatencies = "goproxy.PostSDKLatencies"
const latencyPostSDKCounters = "goproxy.PostSDKCounters"
const latencyPostSDKGauge = "goproxy.PostSDKGague"

//-----------------------------------------------------------------------------
// SPLIT CHANGES
//-----------------------------------------------------------------------------
func fetchSplitsFromDB(since int) ([]json.RawMessage, int64, error) {

	till := int64(since)
	splits := make([]json.RawMessage, 0)

	splitCollection := collections.NewSplitChangesCollection(boltdb.DBB)
	items, err := splitCollection.FetchAll()
	if err != nil {
		log.Error.Println(err)
		return splits, till, err
	}

	for _, split := range items {
		if split.Status == "ACTIVE" && split.ChangeNumber > int64(since) {
			if split.ChangeNumber > till {
				till = split.ChangeNumber
			}
			splits = append(splits, []byte(split.JSON))
		}
	}

	return splits, till, nil
}

func splitChanges(c *gin.Context) {
	sinceParam := c.DefaultQuery("since", "-1")
	since, err := strconv.Atoi(sinceParam)
	if err != nil {
		since = -1
	}

	startTime := controllerLatencies.StartMeasuringLatency()
	splits, till, errf := fetchSplitsFromDB(since)
	if errf != nil {
		log.Error.Println(errf)
		controllerCounters.Increment("splitChangeFetcher.status.500")
		controllerCounters.Increment("splitChangeFetcher.exception")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errf.Error()})
		return
	}
	controllerLatencies.RegisterLatency("splitChangeFetcher.time", startTime)
	controllerLatencies.RegisterLatency(latencyFetchSplitsFromDB, startTime)
	controllerCounters.Increment("splitChangeFetcher.status.200")
	c.JSON(http.StatusOK, gin.H{"splits": splits, "since": since, "till": till})
}

//-----------------------------------------------------------------------------
// SEGMENT CHANGES
//-----------------------------------------------------------------------------

func fetchSegmentsFromDB(since int, segmentName string) ([]string, []string, int64, error) {
	added := make([]string, 0)
	removed := make([]string, 0)
	till := int64(since)

	segmentCollection := collections.NewSegmentChangesCollection(boltdb.DBB)
	item, err := segmentCollection.Fetch(segmentName)
	if err != nil {
		switch err {
		case boltdb.ErrorBucketNotFound:
			log.Warning.Printf("Bucket not found for segment [%s]\n", segmentName)
		default:
			log.Error.Println(err)
		}
		return added, removed, till, err
	}

	for _, skey := range item.Keys {
		if skey.ChangeNumber > int64(since) {
			if skey.Removed {
				if since > 0 {
					removed = append(removed, skey.Name)
				}
			} else {
				added = append(added, skey.Name)
			}

			if since > 0 {
				if skey.ChangeNumber > till {
					till = skey.ChangeNumber
				}
			} else {
				if !skey.Removed && skey.ChangeNumber > till {
					till = skey.ChangeNumber
				}
			}
		}
	}

	return added, removed, till, nil
}

func segmentChanges(c *gin.Context) {
	sinceParam := c.DefaultQuery("since", "-1")
	since, err := strconv.Atoi(sinceParam)
	if err != nil {
		since = -1
	}

	segmentName := c.Param("name")
	startTime := controllerLatencies.StartMeasuringLatency()
	added, removed, till, errf := fetchSegmentsFromDB(since, segmentName)
	if errf != nil {
		controllerCounters.Increment("segmentChangeFetcher.status.500")
		controllerCounters.Increment("segmentChangeFetcher.exception")
		c.JSON(http.StatusInternalServerError, gin.H{"error": errf.Error()})
		return
	}
	controllerLatencies.RegisterLatency("segmentChangeFetcher.time", startTime)
	controllerLatencies.RegisterLatency(latencyFetchSegmentFromDB, startTime)
	controllerCounters.Increment("segmentChangeFetcher.status.200")
	c.JSON(http.StatusOK, gin.H{"name": segmentName, "added": added,
		"removed": removed, "since": since, "till": till})
}

//-----------------------------------------------------------------
//                 I M P R E S S I O N S
//-----------------------------------------------------------------
func postBulkImpressions(c *gin.Context) {
	sdkVersion := c.Request.Header.Get("SplitSDKVersion")
	machineIP := c.Request.Header.Get("SplitSDKMachineIP")
	data, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		log.Error.Println(err)
		c.JSON(http.StatusInternalServerError, nil)
	}
	startTime := controllerLatencies.StartMeasuringLatency()
	controllers.AddImpressions(data, sdkVersion, machineIP)
	controllerLatencies.RegisterLatency(latencyAddImpressionsInBuffer, startTime)
	c.JSON(http.StatusOK, nil)
}

//-----------------------------------------------------------------------------
// METRICS
//-----------------------------------------------------------------------------

func postMetricsTimes(c *gin.Context) {
	startTime := controllerLatencies.StartMeasuringLatency()
	postEvent(c, api.PostMetricsLatency)
	controllerLatencies.RegisterLatency(latencyPostSDKLatencies, startTime)
	c.JSON(http.StatusOK, "")
}

func postMetricsCounters(c *gin.Context) {
	startTime := controllerLatencies.StartMeasuringLatency()
	postEvent(c, api.PostMetricsCounters)
	controllerLatencies.RegisterLatency(latencyPostSDKCounters, startTime)
	c.JSON(http.StatusOK, "")
}

func postMetricsGauge(c *gin.Context) {
	startTime := controllerLatencies.StartMeasuringLatency()
	postEvent(c, api.PostMetricsGauge)
	controllerLatencies.RegisterLatency(latencyPostSDKGauge, startTime)
	c.JSON(http.StatusOK, "")
}

func postEvent(c *gin.Context, fn func([]byte, string, string) error) {
	sdkVersion := c.Request.Header.Get("SplitSDKVersion")
	machineIP := c.Request.Header.Get("SplitSDKMachineIP")
	data, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		log.Error.Println(err)
	}

	// TODO add channel to control number of posts
	go func() {
		log.Debug.Println(sdkVersion, machineIP, string(data))
		var e = fn(data, sdkVersion, machineIP)
		if e != nil {
			log.Error.Println(e)
		}
	}()
}

//-----------------------------------------------------------------------------
// ADMIN
//-----------------------------------------------------------------------------

func uptime(c *gin.Context) {
	upt := stats.Uptime()
	d := int64(0)
	h := int64(0)
	m := int64(0)
	s := int64(upt.Seconds())

	if s > 60 {
		m = int64(s / 60)
		s = s - m*60
	}

	if m > 60 {
		h = int64(m / 60)
		m = m - h*60
	}

	if h > 24 {
		d = int64(h / 24)
		h = h - d*24
	}

	c.JSON(http.StatusOK, gin.H{"uptime": fmt.Sprintf("%dd %dh %dm %ds", d, h, m, s)})
}

func version(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"version": splitio.Version})
}

func ping(c *gin.Context) {
	c.String(http.StatusOK, "%s", "pong")
}

func showStats(c *gin.Context) {
	counters := stats.Counters()
	latencies := stats.Latencies()
	c.JSON(http.StatusOK, gin.H{"counters": counters, "latencies": latencies})
}
