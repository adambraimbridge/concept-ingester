package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	queueConsumer "github.com/Financial-Times/message-queue-gonsumer/consumer"
	metrics "github.com/rcrowley/go-metrics"
	"github.com/stretchr/testify/assert"
)

var peopleServiceAddress = "http://people-rw-neo4j-blue:8080"
var organisationsServiceAddress = "http://organisations-rw-neo4j-blue:8080"

var correctWriterMappings = map[string]string{
	"people-rw-neo4j-blue":        "http://people-rw-neo4j-blue:8080",
	"organisations-rw-neo4j-blue": "http://organisations-rw-neo4j-blue:8080",
}

var uuid = "5e0ad5e5-c3d4-387d-9875-ec15501808e5"
var validMessageTypeOrganisations = "organisations"
var invalidMessageType = "animals"

var validExpression = regexp.MustCompile(`^(?P<protocol>http):\/\/(?P<host>[^:\/\s]+):(?P<port>[\d]{1,5})$`)

func TestMessageProcessingHappyPathIncrementsSuccessMeter(t *testing.T) {
	// Test server that always responds with 200 code
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()

	mockedWriterMappings := map[string]string{
		"people-rw-neo4j-blue":        "http://people-rw-neo4j-blue:8080",
		"organisations-rw-neo4j-blue": server.URL,
	}

	successMeterInitialCount, failureMeterInitialCount := getCounts()

	ing := ingesterService{baseURLMappings: mockedWriterMappings, client: http.Client{}}

	err := ing.processMessage(createMessage(uuid, validMessageTypeOrganisations))

	assertion := assert.New(t)
	assertion.NoError(err, "Should complete without error")

	successMeterFinalCount, failureMeterFinalCount := getCounts()

	assertion.True(successMeterFinalCount-successMeterInitialCount == 1, "Should have incremented SuccessCount by 1")
	assertion.True(failureMeterFinalCount-failureMeterInitialCount == 0, "Should not have incremented FailureCount")
}

func TestMessageProcessingUnhappyPathIncrementsFailureMeter(t *testing.T) {
	// Test server that always responds with 500 code
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer server.Close()

	mockedWriterMappings := map[string]string{
		"people-rw-neo4j-blue":        "http://people-rw-neo4j-blue:8080",
		"organisations-rw-neo4j-blue": server.URL,
	}

	successMeterInitialCount, failureMeterInitialCount := getCounts()

	ing := ingesterService{baseURLMappings: mockedWriterMappings, client: http.Client{}}

	err := ing.processMessage(createMessage(uuid, validMessageTypeOrganisations))

	assertion := assert.New(t)
	assertion.Error(err, "Should error")

	successMeterFinalCount, failureMeterFinalCount := getCounts()

	assertion.True(successMeterFinalCount-successMeterInitialCount == 0, "Should not have incremented SuccessCount")
	assertion.True(failureMeterFinalCount-failureMeterInitialCount == 1, "Should have incremented FailureCount by 1")
}

func TestMessageProcessingHappyPathIncrementsSuccessMeterForElasticsearchIncluded(t *testing.T) {
	// Test server that always responds with 200 code
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer server.Close()

	mockedWriterMappings := map[string]string{
		"people-rw-neo4j-blue":        "http://people-rw-neo4j-blue:8080",
		"organisations-rw-neo4j-blue": server.URL,
	}

	successMeterInitialCount, failureMeterInitialCount := getCounts()
	failureMeterInitialCountForElasticsearch := getElasticsearchCount()

	ing := ingesterService{baseURLMappings: mockedWriterMappings, elasticWriterURL: server.URL, client: http.Client{}}

	err := ing.processMessage(createMessage(uuid, validMessageTypeOrganisations))

	assertion := assert.New(t)
	assertion.NoError(err, "Should complete without error")

	successMeterFinalCount, failureMeterFinalCount := getCounts()
	failureMeterFinalCountForElasticsearch := getElasticsearchCount()

	assertion.True(successMeterFinalCount-successMeterInitialCount == 1, "Should have incremented SuccessCount by 1")
	assertion.True(failureMeterFinalCount-failureMeterInitialCount == 0, "Should not have incremented FailureCount")
	assertion.True(failureMeterFinalCountForElasticsearch-failureMeterInitialCountForElasticsearch == 0, "Should not have incremented FailureCount")
}

func TestMessageProcessingUnhappyPathIncrementsFailureMeterWithElasticsearch(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/bulk") {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer server.Close()

	mockedWriterMappings := map[string]string{
		"people-rw-neo4j-blue":        "http://people-rw-neo4j-blue:8080",
		"organisations-rw-neo4j-blue": server.URL,
	}

	successMeterInitialCount, failureMeterInitialCount := getCounts()
	failureMeterInitialCountForElasticsearch := getElasticsearchCount()

	ing := ingesterService{baseURLMappings: mockedWriterMappings, elasticWriterURL: server.URL + "/bulk", client: http.Client{}}

	err := ing.processMessage(createMessage(uuid, validMessageTypeOrganisations))

	assertion := assert.New(t)
	assertion.Error(err, "Should error")

	successMeterFinalCount, failureMeterFinalCount := getCounts()
	failureMeterFinalCountForElasticsearch := getElasticsearchCount()

	assertion.True(successMeterFinalCount-successMeterInitialCount == 0, "Should not have incremented SuccessCount")
	assertion.True(failureMeterFinalCount-failureMeterInitialCount == 0, "Should not have incremented FailureCount")
	assertion.True(failureMeterFinalCountForElasticsearch-failureMeterInitialCountForElasticsearch == 1, "Should have incremented FailureCount by 1")
}

func getCounts() (int64, int64) {
	successMeter := metrics.GetOrRegisterMeter("organisations-SUCCESS", metrics.DefaultRegistry)
	failureMeter := metrics.GetOrRegisterMeter("organisations-FAILURE", metrics.DefaultRegistry)
	return successMeter.Count(), failureMeter.Count()
}

func getElasticsearchCount() int64 {
	return metrics.GetOrRegisterMeter("organisations-elasticsearch-FAILURE", metrics.DefaultRegistry).Count()
}

func TestSuccessfulElasticsearchWriterMappingsCreation(t *testing.T) {
	elasticsearchWriterBasicMapping, elasticsearchWriterBulkMapping, err := createElasticsearchWriterMappings("http://elasticsearchHost:8080")
	assertion := assert.New(t)
	assertion.NoError(err)
	assertion.EqualValues("http://elasticsearchHost:8080", elasticsearchWriterBasicMapping)
	assertion.EqualValues("http://elasticsearchHost:8080/bulk", elasticsearchWriterBulkMapping)
}

func TestElasticsearchWriterMappingsCreationWithEmptyAddress(t *testing.T) {
	elasticsearchWriterBasicMapping, elasticsearchWriterBulkMapping, err := createElasticsearchWriterMappings("")
	assertion := assert.New(t)
	assertion.NoError(err)
	assertion.Empty(elasticsearchWriterBasicMapping)
	assertion.Empty(elasticsearchWriterBulkMapping)
}

func TestUnsuccessfulElasticsearchWriterMappingsCreation(t *testing.T) {
	_, _, err := createElasticsearchWriterMappings("http://invalidAddress:8080/invalid")
	assertion := assert.New(t)
	assertion.Error(err)
}

func TestSuccessfulWriterMappingsCreation(t *testing.T) {
	writerMappings, err := createWriterMappings(peopleServiceAddress + "," + organisationsServiceAddress)
	assertion := assert.New(t)
	assertion.NoError(err)
	assertion.EqualValues(correctWriterMappings, writerMappings, "Should have two mappings")
}

func TestUnsuccessfulWriterMappingsCreation(t *testing.T) {
	_, err := createWriterMappings("http://invalidAddress:8080/invalid")
	assertion := assert.New(t)
	assertion.Error(err)
}

func TestSuccessfulAddressComponentsExtraction(t *testing.T) {
	testCases := []struct {
		address            string
		expectedComponents []string
	}{
		{"http://localhost:1", []string{"http", "localhost", "1"}},
		{"http://localhost:12", []string{"http", "localhost", "12"}},
		{"http://localhost:123", []string{"http", "localhost", "123"}},
		{"http://localhost:1234", []string{"http", "localhost", "1234"}},
		{"http://localhost:12345", []string{"http", "localhost", "12345"}},
		{"http://com.ft.address:8080", []string{"http", "com.ft.address", "8080"}},
	}

	assertion := assert.New(t)
	for _, tc := range testCases {
		components, err := extractAddressComponents(validExpression, tc.address)
		assertion.NoError(err)
		assertion.True(components["protocol"] == tc.expectedComponents[0], "Components must contain 'protocol' key equal to:"+tc.expectedComponents[0])
		assertion.True(components["host"] == tc.expectedComponents[1], "Components must contain 'host' key equal to:"+tc.expectedComponents[1])
		assertion.True(components["port"] == tc.expectedComponents[2], "Components must contain 'port' key equal to:"+tc.expectedComponents[2])
	}
}

func TestUnsuccessfulAddressComponentsExtraction(t *testing.T) {
	testCases := []struct {
		address string
	}{
		{"tcp://localhost:8080"},
		{"http://:8080"},
		{"http://localhost:"},
		{"http://localhost:8080/"},
		{"http://localhost:8080/path"},
		{"http://:"},
		{"://localhost:8080"},
		{"//localhost:8080"},
		{"localhost:8080"},
		{":"},
		{"http://localhost::8080"},
		{"http:://localhost:8080"},
		{"http://localhost:8080:"},
		{"http:/localhost:8080"},
		{"http:localhost:8080"},
		{"http:localhost:123456"},
	}

	assertion := assert.New(t)
	for _, tc := range testCases {
		_, err := extractAddressComponents(validExpression, tc.address)
		assertion.Error(err)
	}
}

func TestUuidAndMessageTypeAreExtractedFromMessage(t *testing.T) {
	validMessage := createMessage(uuid, validMessageTypeOrganisations)
	extractedIngestionType, extractedUUID := extractMessageTypeAndId(validMessage.Headers)
	assertion := assert.New(t)
	assertion.Equal("organisations", extractedIngestionType)
	assertion.Equal("5e0ad5e5-c3d4-387d-9875-ec15501808e5", extractedUUID)
}

func TestWriterUrlIsResolvedCorrectlyAndRequestIsNotNull(t *testing.T) {
	validMessage := createMessage(uuid, validMessageTypeOrganisations)
	writerUrl, err := resolveWriter(validMessageTypeOrganisations, correctWriterMappings)
	request, reqURL, err := createWriteRequest(validMessageTypeOrganisations, strings.NewReader(validMessage.Body), uuid, writerUrl)
	assertion := assert.New(t)
	assertion.NoError(err)
	assertion.Equal("http://organisations-rw-neo4j-blue:8080/organisations/5e0ad5e5-c3d4-387d-9875-ec15501808e5", reqURL)
	assertion.NotNil(request, "Request is nil")
}

func TestErrorIsThrownWhenIngestionTypeMatchesNoWriters(t *testing.T) {
	writerUrl, err := resolveWriter(invalidMessageType, correctWriterMappings)
	assertion := assert.New(t)
	assertion.Equal("", writerUrl)
	assertion.Error(err, "No configured writer for concept: "+invalidMessageType)
}

func TestOutputMetricsIfRequiredWithInvalidAddress(t *testing.T) {
	invalidProtocolAddress := "http://localhost:8080"
	err := outputMetricsIfRequired(invalidProtocolAddress, "graphite-prefix", false)
	assertion := assert.New(t)
	assertion.Error(err)
}

func createMessage(messageID string, messageType string) queueConsumer.Message {
	return queueConsumer.Message{
		Headers: map[string]string{
			"Content-Type":      "application/json",
			"Message-Id":        messageID,
			"Message-Timestamp": "2016-06-16T08:14:36.910Z",
			"Message-Type":      messageType,
			"Origin-System-Id":  "http://cmdb.ft.com/systems/upp",
			"X-Request-Id":      "tid_newid",
		},
		Body: `{transformed-org-json`}
}
