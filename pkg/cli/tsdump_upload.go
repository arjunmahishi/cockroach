// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cli

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/ts"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"gopkg.in/yaml.v3"
)

const (
	DatadogSeriesTypeUnknown = iota
	DatadogSeriesTypeCounter
	DatadogSeriesTypeRate
	DatadogSeriesTypeGauge
)
const (
	UploadStatusSuccess = "Success"
	UploadStatusFailure = "Failed"
	nodeKey             = "node_id"
)

var (
	// each site in datadog has a different host name. ddSiteToHostMap
	// holds the mapping of site name to the host name.
	ddSiteToHostMap = map[string]string{
		"us1":     "datadoghq.com",
		"us3":     "us3.datadoghq.com",
		"us5":     "us5.datadoghq.com",
		"eu1":     "datadoghq.eu",
		"ap1":     "ap1.datadoghq.com",
		"us1-fed": "ddog-gov.com",
	}

	targetURLFormat           = "https://api.%s/api/v2/series"
	datadogDashboardURLFormat = "https://us5.datadoghq.com/dashboard/bif-kwe-gx2/self-hosted-db-console-tsdump?" +
		"tpl_var_cluster=%s&tpl_var_upload_id=%s&tpl_var_upload_day=%d&tpl_var_upload_month=%d&tpl_var_upload_year=%d&from_ts=%d&to_ts=%d"
	zipFileSignature            = []byte{0x50, 0x4B, 0x03, 0x04}
	logMessageFormat            = "tsdump upload to datadog is partially failed for metric: %s"
	partialFailureMessageFormat = "The Tsdump upload to Datadog succeeded but %d metrics partially failed to upload." +
		" These failures can be due to transietnt network errors. If any of these metrics are critical for your investigation," +
		" please re-upload the Tsdump:\n%s\n"
	datadogLogsURLFormat = "https://us5.datadoghq.com/logs?query=cluster_label:%s+upload_id:%s"

	translateMetricType = map[string]int{
		"GAUGE":   DatadogSeriesTypeGauge,
		"COUNTER": DatadogSeriesTypeCounter,
	}
)

// DatadogPoint is a single metric point in Datadog format
type DatadogPoint struct {
	// Timestamp must be in seconds since Unix epoch.
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

// DatadogSeries contains a JSON encoding of a single series object
// that can be send to Datadog.
type DatadogSeries struct {
	Metric    string         `json:"metric"`
	Type      int            `json:"type"`
	Points    []DatadogPoint `json:"points"`
	Interval  int            `json:"interval,omitempty"`
	Resources []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"resources"`
	// In order to encode arbitrary key-value pairs, use a `:` delimited
	// tag string like `cluster:dedicated`.
	Tags []string `json:"tags"`
}

// DatadogSubmitMetrics is the top level JSON object that must be sent to Datadog.
// See: https://docs.datadoghq.com/api/latest/metrics/#submit-metrics
type DatadogSubmitMetrics struct {
	Series []DatadogSeries `json:"series"`
}

type DatadogResp struct {
	Errors []string `json:"errors"`
}

var newTsdumpUploadID = func(uploadTime time.Time) string {
	clusterTagValue := "cluster-debug"
	if debugTimeSeriesDumpOpts.clusterLabel != "" {
		clusterTagValue = debugTimeSeriesDumpOpts.clusterLabel
	}
	return newUploadID(clusterTagValue, uploadTime)
}

// datadogWriter can convert our metrics to Datadog format and send
// them via HTTP to the public DD endpoint, assuming an API key is set
// in the CLI flags.
type datadogWriter struct {
	sync.Once
	targetURL string
	uploadID  string
	init      bool
	apiKey    string
	// namePrefix sets the string to prepend to all metric names. The
	// names are kept with `.` delimiters.
	namePrefix     string
	doRequest      func(req *http.Request) error
	threshold      int
	uploadTime     time.Time
	storeToNodeMap map[string]string
	metricTypeMap  map[string]string
}

func makeDatadogWriter(
	targetURL string,
	init bool,
	apiKey string,
	threshold int,
	doRequest func(req *http.Request) error,
) *datadogWriter {
	currentTime := getCurrentTime()

	var metricTypeMap map[string]string
	if init {
		// we only need to load the metric types map when the command is
		// datadogInit. It's ok to keep it nil otherwise.
		var err error
		metricTypeMap, err = loadMetricTypesMap(context.Background())
		if err != nil {
			fmt.Printf(
				"error loading metric types map: %v\nThis may lead to some metrics not behaving correctly on Datadog.\n", err)
		}
	}

	return &datadogWriter{
		targetURL:      targetURL,
		uploadID:       newTsdumpUploadID(currentTime),
		init:           init,
		apiKey:         apiKey,
		namePrefix:     "crdb.tsdump.", // Default pre-set prefix to distinguish these uploads.
		doRequest:      doRequest,
		threshold:      threshold,
		uploadTime:     currentTime,
		storeToNodeMap: make(map[string]string),
		metricTypeMap:  metricTypeMap,
	}
}

var getCurrentTime = func() time.Time {
	return timeutil.Now()
}

var doDDRequest = func(req *http.Request) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	ddResp := DatadogResp{}
	err = json.Unmarshal(respBytes, &ddResp)
	if err != nil {
		return err
	}
	if len(ddResp.Errors) > 0 {
		return errors.Newf("tsdump: error response from datadog (%d): %+v; request: %+v", resp.StatusCode, ddResp.Errors, req)
	}
	if resp.StatusCode > 299 {
		return errors.Newf("tsdump: bad response status code: %+v", resp)
	}
	return nil
}

// appendTag appends a formatted tag to the series tags.
func appendTag(series *DatadogSeries, tagKey, tagValue string) {
	series.Tags = append(series.Tags, fmt.Sprintf("%s:%s", tagKey, tagValue))
}

func (d *datadogWriter) dump(kv *roachpb.KeyValue) (*DatadogSeries, error) {
	name, source, res, _, err := ts.DecodeDataKey(kv.Key)
	if err != nil {
		return nil, err
	}
	var idata roachpb.InternalTimeSeriesData
	if err := kv.Value.GetProto(&idata); err != nil {
		return nil, err
	}

	series := &DatadogSeries{
		Metric:   name,
		Tags:     []string{},
		Type:     d.resolveMetricType(name),
		Points:   make([]DatadogPoint, idata.SampleCount()),
		Interval: int(res.Duration().Seconds()), // convert from time.Duration to number of seconds.
	}

	sl := reCrStoreNode.FindStringSubmatch(name)
	if len(sl) >= 3 {
		// extract the node/store and metric name from the regex match.
		key := sl[1]
		series.Metric = sl[2]

		switch key {
		case "node":
			appendTag(series, nodeKey, source)
		case "store":
			appendTag(series, key, source)
			// We check the node associated with store if store to node mapping
			// is provided as part of --store-to-node-map-file flag. If exists then
			// emit node as tag.
			if nodeID, ok := d.storeToNodeMap[source]; ok {
				appendTag(series, nodeKey, nodeID)
			}
		default:
			appendTag(series, key, source)
		}
	} else {
		// add default node_id as 0 as there is no metric match for the regex.
		appendTag(series, nodeKey, "0")
	}

	for i := 0; i < idata.SampleCount(); i++ {
		if idata.IsColumnar() {
			series.Points[i].Timestamp = idata.TimestampForOffset(idata.Offset[i]) / 1_000_000_000
			series.Points[i].Value = idata.Last[i]
		} else {
			series.Points[i].Timestamp = idata.TimestampForOffset(idata.Samples[i].Offset) / 1_000_000_000
			series.Points[i].Value = idata.Samples[i].Sum
		}

	}
	return series, nil
}

func (d *datadogWriter) resolveMetricType(metricName string) int {
	if !d.init {
		// in this is not datadogInit command, we don't need to resolve the metric
		// type. We can just return DatadogSeriesTypeUnknown. Datadog only expects
		// us to send the type information only once.
		return DatadogSeriesTypeUnknown
	}

	typeLookupKey := strings.TrimPrefix(metricName, "cr.store.")
	typeLookupKey = strings.TrimPrefix(typeLookupKey, "cr.node.")
	metricType := d.metricTypeMap[typeLookupKey]
	if t, ok := translateMetricType[metricType]; ok {
		return t
	}

	if strings.HasSuffix(metricName, "-count") {
		return DatadogSeriesTypeCounter
	}

	if strings.HasSuffix(metricName, "-avg") ||
		strings.HasSuffix(metricName, "-max") ||
		strings.HasSuffix(metricName, "-sum") ||
		strings.HasSuffix(metricName, "-p50") ||
		strings.HasSuffix(metricName, "-p75") ||
		strings.HasSuffix(metricName, "-p90") ||
		strings.HasSuffix(metricName, "-p99") ||
		strings.HasSuffix(metricName, "-p99.9") ||
		strings.HasSuffix(metricName, "-p99.99") ||
		strings.HasSuffix(metricName, "-p99.999") {
		return DatadogSeriesTypeGauge
	}

	return DatadogSeriesTypeUnknown
}

var printLock syncutil.Mutex

func (d *datadogWriter) emitDataDogMetrics(data []DatadogSeries) ([]string, error) {
	tags := getUploadTags(d)

	emittedMetrics := make([]string, len(data))
	for i := 0; i < len(data); i++ {
		data[i].Tags = append(data[i].Tags, tags...)
		data[i].Metric = d.namePrefix + data[i].Metric
		emittedMetrics[i] = data[i].Metric
	}

	// When running in init mode, we insert zeros with the current
	// timestamp in order to populate Datadog's metrics list. Then the
	// user can enable these metrics for historic ingest and load the
	// full dataset. This should only be necessary once globally.
	if d.init {
		for i := 0; i < len(data); i++ {
			data[i].Points = []DatadogPoint{{
				Value:     0,
				Timestamp: getCurrentTime().Unix(),
			}}
		}
	}

	// Because the log below resets and overwrites the print line in
	// order to avoid an explosion of logs filling the screen, it's
	// necessary to wrap it in a mutex since `emitDataDogMetrics` is
	// called concurrently.
	func() {
		printLock.Lock()
		defer printLock.Unlock()
		if len(data) > 0 {
			fmt.Printf(
				"\033[G\033[Ktsdump datadog upload: uploading metrics containing %d series including %s",
				len(data),
				data[0].Metric,
			)
		} else {
			fmt.Printf(
				"\033[G\033[Ktsdump datadog upload: uploading metrics containing 0 series",
			)
		}
	}()

	return emittedMetrics, d.flush(data)
}

func getUploadTags(d *datadogWriter) []string {
	var tags []string
	// Hardcoded values
	tags = append(tags, "cluster_type:SELF_HOSTED")

	if debugTimeSeriesDumpOpts.clusterLabel != "" {
		tags = append(tags, makeDDTag("cluster_label", debugTimeSeriesDumpOpts.clusterLabel))
	}
	if debugTimeSeriesDumpOpts.clusterID != "" {
		tags = append(tags, makeDDTag("cluster_id", debugTimeSeriesDumpOpts.clusterID))
	}
	if debugTimeSeriesDumpOpts.zendeskTicket != "" {
		tags = append(tags, makeDDTag("zendesk_ticket", debugTimeSeriesDumpOpts.zendeskTicket))
	}
	if debugTimeSeriesDumpOpts.organizationName != "" {
		tags = append(tags, makeDDTag("org_name", debugTimeSeriesDumpOpts.organizationName))
	}
	if debugTimeSeriesDumpOpts.userName != "" {
		tags = append(tags, makeDDTag("user_name", debugTimeSeriesDumpOpts.userName))
	}

	tags = append(tags, makeDDTag(uploadIDTag, d.uploadID))

	year, month, day := d.uploadTime.Date()
	tags = append(tags, makeDDTag("upload_timestamp", d.uploadTime.Format("2006-01-02 15:04:05")))
	tags = append(tags, makeDDTag("upload_year", strconv.Itoa(year)))
	tags = append(tags, makeDDTag("upload_month", strconv.Itoa(int(month))))
	tags = append(tags, makeDDTag("upload_day", strconv.Itoa(day)))
	return tags
}

func (d *datadogWriter) flush(data []DatadogSeries) error {
	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(&DatadogSubmitMetrics{Series: data})
	if err != nil {
		return err
	}
	var zipBuf bytes.Buffer
	g := gzip.NewWriter(&zipBuf)
	_, err = io.Copy(g, &buf)
	if err != nil {
		return err
	}
	err = g.Close()
	if err != nil {
		return err
	}

	retryOpts := base.DefaultRetryOptions()
	retryOpts.MaxBackoff = 100 * time.Millisecond
	retryOpts.MaxRetries = 100
	var req *http.Request
	for retry := retry.Start(retryOpts); retry.Next(); {
		req, err = http.NewRequest("POST", d.targetURL, &zipBuf)
		if err != nil {
			return err
		}
		req.Header.Set("DD-API-KEY", d.apiKey)
		req.Header.Set(server.ContentTypeHeader, "application/json")
		req.Header.Set(httputil.ContentEncodingHeader, "gzip")

		err = d.doRequest(req)
		if err == nil {
			return nil
		}
	}
	return err

}

func (d *datadogWriter) upload(fileName string) error {
	f, err := getFileReader(fileName)
	if err != nil {
		return err
	}

	storeToNodeYamlFile := debugTimeSeriesDumpOpts.storeToNodeMapYAMLFile
	if storeToNodeYamlFile != "" {
		d.populateNodeAndStoreMap(storeToNodeYamlFile)
	}

	dec := gob.NewDecoder(f)
	decodeOne := func() ([]DatadogSeries, error) {
		var ddSeries []DatadogSeries

		for i := 0; i < d.threshold; i++ {
			var v roachpb.KeyValue
			err := dec.Decode(&v)
			if err != nil {
				return ddSeries, err
			}

			datadogSeries, err := d.dump(&v)
			if err != nil {
				return nil, err
			}
			ddSeries = append(ddSeries, *datadogSeries)
		}

		return ddSeries, nil
	}

	var wg sync.WaitGroup
	ch := make(chan []DatadogSeries, 4000)

	// metricsUploadState wraps the failed metrics collection in a mutex since
	// they're collected concurrently.
	var metricsUploadState struct {
		syncutil.Mutex
		// we are taking map here as we want unique metric names which were failed across all tsdump upload requests.
		uploadFailedMetrics     map[string]struct{}
		isSingleUploadSucceeded bool
	}
	metricsUploadState.uploadFailedMetrics = make(map[string]struct{})
	metricsUploadState.isSingleUploadSucceeded = false

	markSuccessOnce := sync.OnceFunc(func() {
		metricsUploadState.isSingleUploadSucceeded = true
	})

	// Note(davidh): This was previously set at 1000 and we'd get regular
	// 400s from Datadog with the cryptic `Unable to decompress payload`
	// error. We reduced this to 20 and was able to upload a 3.2GB tsdump
	// in 6m20s without any errors.
	for i := 0; i < 20; i++ {
		go func() {
			for data := range ch {
				emittedMetrics, err := d.emitDataDogMetrics(data)
				if err != nil {
					func() {
						metricsUploadState.Lock()
						defer metricsUploadState.Unlock()
						for _, metric := range emittedMetrics {
							metricsUploadState.uploadFailedMetrics[metric] = struct{}{}
						}
					}()
				} else {
					func() {
						markSuccessOnce()
					}()
				}
				wg.Done()
			}
		}()
	}

	for {
		data, err := decodeOne()
		if err == io.EOF {
			if len(data) != 0 {
				wg.Add(1)
				ch <- data
			}
			break
		}
		wg.Add(1)
		ch <- data
	}

	wg.Wait()
	toUnixTimestamp := timeutil.Now().UnixMilli()
	//create timestamp for T-30 days.
	fromUnixTimestamp := toUnixTimestamp - (30 * 24 * 60 * 60 * 1000)
	year, month, day := d.uploadTime.Date()
	dashboardLink := fmt.Sprintf(datadogDashboardURLFormat, debugTimeSeriesDumpOpts.clusterLabel, d.uploadID, day, int(month), year, fromUnixTimestamp, toUnixTimestamp)

	var uploadStatus string
	if metricsUploadState.isSingleUploadSucceeded {
		uploadStatus = UploadStatusSuccess
	} else {
		uploadStatus = UploadStatusFailure
	}
	fmt.Printf("\nUpload status: %s!\n", uploadStatus)

	if metricsUploadState.isSingleUploadSucceeded {
		var isDatadogUploadFailed = false
		markDatadogUploadFailedOnce := sync.OnceFunc(func() {
			isDatadogUploadFailed = true
		})
		if len(metricsUploadState.uploadFailedMetrics) != 0 {
			fmt.Printf(partialFailureMessageFormat, len(metricsUploadState.uploadFailedMetrics), strings.Join(func() []string {
				var failedMetricsList []string
				index := 1
				for metric := range metricsUploadState.uploadFailedMetrics {
					metric = fmt.Sprintf("%d) %s", index, metric)
					failedMetricsList = append(failedMetricsList, metric)
					index++
				}
				return failedMetricsList
			}(), "\n"))

			tags := strings.Join(getUploadTags(d), ",")
			fmt.Println("\nPushing logs of metric upload failures to datadog...")
			for metric := range metricsUploadState.uploadFailedMetrics {
				wg.Add(1)
				go func(metric string) {
					logMessage := fmt.Sprintf(logMessageFormat, metric)

					logEntryJSON, _ := json.Marshal(struct {
						Message any    `json:"message,omitempty"`
						Tags    string `json:"ddtags,omitempty"`
						Source  string `json:"ddsource,omitempty"`
					}{
						Message: logMessage,
						Tags:    tags,
						Source:  "tsdump_upload",
					})

					_, err := uploadLogsToDatadog(logEntryJSON, d.apiKey, debugTimeSeriesDumpOpts.ddSite)
					if err != nil {
						markDatadogUploadFailedOnce()
					}
					wg.Done()
				}(metric)
			}

			wg.Wait()
			if isDatadogUploadFailed {
				fmt.Println("Failed to pushed some metrics to datadog logs. Please refer CLI output for all failed metrics.")
			} else {
				fmt.Println("Pushing logs of metric upload failures to datadog...done")
				fmt.Printf("datadog logs for metric upload failures link: %s\n", fmt.Sprintf(datadogLogsURLFormat, debugTimeSeriesDumpOpts.clusterLabel, d.uploadID))
			}
		}
		fmt.Println("\nupload id:", d.uploadID)
		fmt.Printf("datadog dashboard link: %s\n", dashboardLink)
	} else {
		fmt.Println("All metric upload is failed. Please re-upload the Tsdump.")
	}

	close(ch)
	return nil
}

func (d *datadogWriter) populateNodeAndStoreMap(fileName string) {
	file, err := os.Open(fileName)
	if err != nil {
		fmt.Printf("error in opening store to node mapping YAML file: %v\n", err)
		return
	}

	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			fmt.Printf("error in clsoing store to node mapping YAML: %v\n", err)
		}
	}(file)

	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(&d.storeToNodeMap); err != nil {
		fmt.Printf("error decoding store to node mapping file YAML: %v\n", err)
		return
	}
}

// getFileReader returns an io.Reader based on the file type.
func getFileReader(fileName string) (io.Reader, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}

	// Check if the file is a zip file by reading its magic number
	buf := make([]byte, 4)
	if _, err := file.Read(buf); err != nil {
		return nil, err
	}

	// Reset the file pointer to the beginning
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	// Check for zip file signature
	if bytes.HasPrefix(buf, zipFileSignature) {
		zipReader, err := zip.NewReader(file, fileSize(file))
		if err != nil {
			return nil, err
		}

		if len(zipReader.File) > 0 {
			if len(zipReader.File) > 1 {
				fmt.Printf("tsdump datadog upload: warning: more than one file in zip archive, using the first file %s\n", zipReader.File[0].Name)
			}
			firstFile, err := zipReader.File[0].Open()
			if err != nil {
				return nil, err
			}
			return firstFile, nil
		}
		return nil, fmt.Errorf("zip archive is empty")
	}

	return file, nil
}

// fileSize returns the size of the file.
func fileSize(file *os.File) int64 {
	info, err := file.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

func loadMetricTypesMap(ctx context.Context) (map[string]string, error) {
	metricLayers, err := generateMetricList(ctx, true /* skipFiltering */)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate metric list")
	}

	metricTypeMap := make(map[string]string)
	for _, metric := range metricLayers {
		for _, catagory := range metric.Categories {
			for _, m := range catagory.Metrics {
				metricTypeMap[m.Name] = m.Type
			}
		}
	}

	return metricTypeMap, nil
}
