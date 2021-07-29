package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/splitio/go-split-commons/v4/conf"
	"github.com/splitio/go-split-commons/v4/dtos"
	"github.com/splitio/go-split-commons/v4/provisional"
	"github.com/splitio/go-split-commons/v4/service"
	"github.com/splitio/go-split-commons/v4/storage"
	"github.com/splitio/go-split-commons/v4/telemetry"
	commonToolkit "github.com/splitio/go-toolkit/v5/common"
	"github.com/splitio/go-toolkit/v5/logging"
	"github.com/splitio/split-synchronizer/v4/appcontext"
	"github.com/splitio/split-synchronizer/v4/splitio"
	"github.com/splitio/split-synchronizer/v4/splitio/common"
	"github.com/splitio/split-synchronizer/v4/splitio/task"
)

const (
	impressionObserverCacheSize = 500000
)

// RecorderImpressionMultiple struct for impression sync
type RecorderImpressionMultiple struct {
	impressionStorage         storage.ImpressionStorageConsumer
	impressionRecorder        service.ImpressionsRecorder
	localTelemetry            storage.TelemetryRuntimeProducer
	impressionListenerEnabled bool
	logger                    logging.LoggerInterface
	impressionManager         provisional.ImpressionManager
	mode                      string
}

// NewImpressionRecordMultiple creates new impression synchronizer for posting impressions
func NewImpressionRecordMultiple(
	impressionStorage storage.ImpressionStorageConsumer,
	impressionRecorder service.ImpressionsRecorder,
	localTelemetry storage.TelemetryRuntimeProducer,
	logger logging.LoggerInterface,
	managerConfig conf.ManagerConfig,
	impressionsCounter *provisional.ImpressionsCounter,
) (*RecorderImpressionMultiple, error) {
	impressionManager, err := provisional.NewImpressionManager(managerConfig, impressionsCounter, localTelemetry)
	if err != nil {
		return nil, err
	}
	return &RecorderImpressionMultiple{
		impressionStorage:         impressionStorage,
		impressionRecorder:        impressionRecorder,
		localTelemetry:            localTelemetry,
		impressionListenerEnabled: managerConfig.ListenerEnabled,
		logger:                    logger,
		impressionManager:         impressionManager,
		mode:                      managerConfig.ImpressionsMode,
	}, nil
}

func (r *RecorderImpressionMultiple) wrapDTO(collectedData map[dtos.Metadata]map[string][]dtos.ImpressionDTO) map[dtos.Metadata][]dtos.ImpressionsDTO {
	var err error
	impressions := make(map[dtos.Metadata][]dtos.ImpressionsDTO)
	for metadata, impsForMetadata := range collectedData {
		impressions[metadata], err = toImpressionsDTO(impsForMetadata)
		if err != nil {
			r.logger.Error(fmt.Sprintf("Unable to write impressions for metadata %v", metadata))
			continue
		}
	}
	return impressions
}

func (r *RecorderImpressionMultiple) fetch(bulkSize int64) (map[dtos.Metadata][]dtos.ImpressionsDTO, map[dtos.Metadata][]common.ImpressionsListener, error) {
	storedImpressions, err := r.impressionStorage.PopNWithMetadata(bulkSize) // PopN has a mutex, so this function can be async without issues
	if err != nil {
		r.logger.Error("(Task) Post Impressions fails fetching impressions from storage", err.Error())
		return nil, nil, err
	}

	// grouping the information by instanceID/instanceIP, and then by feature name
	collectedDataforLog := make(map[dtos.Metadata]map[string][]dtos.ImpressionDTO)
	collectedDataforListener := make(map[dtos.Metadata]map[string][]common.ImpressionListener)

	for _, stored := range storedImpressions {
		toSend, forListener := r.impressionManager.ProcessImpressions([]dtos.Impression{stored.Impression})

		collectedDataforLog = wrapData(toSend, collectedDataforLog, stored.Metadata)
		collectedDataforListener = wrapDataForListener(forListener, collectedDataforListener, stored.Metadata)
	}

	return r.wrapDTO(collectedDataforLog), wrapDTOListener(collectedDataforListener), nil
}

func (r *RecorderImpressionMultiple) recordImpressions(impressionsToSend map[dtos.Metadata][]dtos.ImpressionsDTO) error {
	for metadata, impressions := range impressionsToSend {
		before := time.Now()
		if appcontext.ExecutionMode() == appcontext.ProducerMode {
			task.StoreDataFlushed(before.UnixNano(), len(impressions), r.impressionStorage.Count(), "impressions")
		}
		err := commonToolkit.WithAttempts(3, func() error {
			r.logger.Debug("impressionsToSend: ", len(impressions))
			err := r.impressionRecorder.Record(impressions, metadata, map[string]string{"SplitSDKImpressionsMode": r.mode})
			if err != nil {
				r.logger.Error("Error posting impressions")
			}

			return nil
		})
		if err != nil {
			if httpError, ok := err.(*dtos.HTTPError); ok {
				r.localTelemetry.RecordSyncError(telemetry.ImpressionSync, httpError.Code)
			}
			return err
		}
		r.localTelemetry.RecordSyncLatency(telemetry.ImpressionSync, time.Now().Sub(before))
		r.localTelemetry.RecordSuccessfulSync(telemetry.ImpressionSync, time.Now().UTC())
	}
	return nil
}

func (r *RecorderImpressionMultiple) sendDataToListener(impressionsToListener map[dtos.Metadata][]common.ImpressionsListener) {
	for metadata, impressions := range impressionsToListener {
		rawImpressions, err := json.Marshal(impressions)
		if err != nil {
			r.logger.Error("JSON encoding failed for the following impressions", impressions)
			continue
		}
		err = task.QueueImpressionsForListener(&task.ImpressionBulk{
			Data:        json.RawMessage(rawImpressions),
			SdkVersion:  metadata.SDKVersion,
			MachineIP:   metadata.MachineIP,
			MachineName: metadata.MachineName,
		})
		if err != nil {
			r.logger.Error(err)
		}
	}
}

func (r *RecorderImpressionMultiple) synchronizeImpressions(bulkSize int64) error {
	impressionsToSend, impressionsForListener, err := r.fetch(bulkSize)
	if err != nil {
		return err
	}

	err = r.recordImpressions(impressionsToSend)
	if err != nil {
		return err
	}
	if r.impressionListenerEnabled {
		r.sendDataToListener(impressionsForListener)
	}
	return nil
}

// SynchronizeImpressions syncs impressions
func (r *RecorderImpressionMultiple) SynchronizeImpressions(bulkSize int64) error {
	if task.IsOperationRunning(task.ImpressionsOperation) {
		r.logger.Debug("Another task executed by the user is performing operations on Impressions. Skipping.")
		return nil
	}

	return r.synchronizeImpressions(bulkSize)
}

// FlushImpressions flushes impressions
func (r *RecorderImpressionMultiple) FlushImpressions(bulkSize int64) error {
	if task.RequestOperation(task.ImpressionsOperation) {
		defer task.FinishOperation(task.ImpressionsOperation)
	} else {
		r.logger.Debug("Cannot execute flush. Another operation is performing operations on Impressions.")
		return errors.New("Cannot execute flush. Another operation is performing operations on Impressions")
	}
	elementsToFlush := splitio.MaxSizeToFlush

	if bulkSize != 0 {
		elementsToFlush = bulkSize
	}

	for elementsToFlush > 0 && r.impressionStorage.Count() > 0 {
		maxSize := splitio.DefaultSize
		if elementsToFlush < splitio.DefaultSize {
			maxSize = elementsToFlush
		}
		err := r.synchronizeImpressions(maxSize)
		if err != nil {
			return err
		}
		elementsToFlush = elementsToFlush - splitio.DefaultSize
	}
	return nil
}
