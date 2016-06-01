package main

import (
	"net/http"
	"os"

	"fmt"
	queueConsumer "github.com/Financial-Times/message-queue-gonsumer/consumer"
	"strings"

	"github.com/Financial-Times/go-fthealth/v1a"
	"github.com/Financial-Times/http-handlers-go/httphandlers"
	status "github.com/Financial-Times/service-status-go/httphandlers"
	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/jawher/mow.cli"
	"github.com/rcrowley/go-metrics"
	"os/signal"
	"sync"
	"syscall"
	"errors"
	"strconv"
)

func main() {
	//TODO debug or Info?
	log.SetLevel(log.InfoLevel)
	app := cli.App("concept-ingester", "A microservice that consumes concept messages from Kafka and routes them to the appropriate writer")
	services := app.StringOpt("services-list", "services", "writer services")
	port := app.StringOpt("port", "8080", "Port to listen on")

	consumerAddrs := app.StringOpt("consumer_proxy_addr", "https://proxy-address", "Comma separated kafka proxy hosts for message consuming.")
	consumerGroupID := app.StringOpt("consumer_group_id", "idiConcept", "Kafka group id used for message consuming.")
	consumerQueue := app.StringOpt("consumer_queue_id", "kafka", "Sets host header")
	consumerOffset := app.StringOpt("consumer_offset", "smallest", "Kafka read offset.")
	consumerAutoCommitEnable := app.BoolOpt("consumer_autocommit_enable", true, "Enable autocommit for small messages.")
	consumerStreamCount := app.IntOpt("consumer_stream_count", 10, "Number of consumer streams")

	topic := app.StringOpt("topic", "Concept", "Kafka topic subscribed to")

	//TODO can we use custom headers
	messageTypeEndpointsMap := map[string]string {
		"organisationIngestion":"organisations",
		"personIngestion":"people",
		"membershipIngestion":"memberships",
		"roleIngestion":"roles",
		"brandIngestion":"brands",
		"subjectIngestion":"subjects",
		"topicIngestion":"topics",
		"sectionIngestion":"sections",
		"genreIngestion":"genre",
		"locationIngestion":"locations",
	}

	app.Action = func() {
		servicesMap := createServicesMap(*services, messageTypeEndpointsMap)
		httpConfigurations := httpConfigurations{baseUrlMap:servicesMap}
		log.Infof("concept-ingester-go-app will listen on port: %s", *port)

		consumerConfig := queueConsumer.QueueConfig{}
		consumerConfig.Addrs = strings.Split(*consumerAddrs, ",")
		consumerConfig.Group = *consumerGroupID
		consumerConfig.Queue = *consumerQueue
		consumerConfig.Topic = *topic
		consumerConfig.Offset = *consumerOffset
		consumerConfig.StreamCount = *consumerStreamCount
		consumerConfig.AutoCommitEnable = *consumerAutoCommitEnable

		client := http.Client{}

		httpConfigurations.client = client
		consumer := queueConsumer.NewConsumer(consumerConfig, httpConfigurations.readMessage, client)

		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			consumer.Start()
			wg.Done()
		}()

		go runServer(httpConfigurations.baseUrlMap, *port)

		ch := make(chan os.Signal)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

		<-ch
		log.Println("Shutting down application...")

		consumer.Stop()
		wg.Wait()

		log.Println("Application closing")
	}
	//TODO debug or Info?
	log.SetLevel(log.DebugLevel)
	log.Infof("Application started with args %s", os.Args)
	app.Run(os.Args)
}

func createServicesMap(services string, messageTypeMap map[string]string) (map[string]string){
	stringSlice := strings.Split(services, ",")
	servicesMap := make(map[string]string)
	for _, service := range stringSlice {
		for messageType, concept := range messageTypeMap {
			if strings.Contains(service, concept) {
				//TODO hardcoded url?
				writerUrl := "http://localhost:8080/__" + service + "/" + concept
				servicesMap[messageType] = writerUrl
				fmt.Printf("Added url %v to map:\n", writerUrl)
			}
		}
	}
	return servicesMap
}

func runServer(baseUrlMap map[string]string, port string) {

	httpHandlers := httpHandlers{baseUrlMap}

	r := router(httpHandlers)
	// The following endpoints should not be monitored or logged (varnish calls one of these every second, depending on config)
	// The top one of these build info endpoints feels more correct, but the lower one matches what we have in Dropwizard,
	// so it's what apps expect currently same as ping, the content of build-info needs more definition
	http.HandleFunc(status.PingPath, status.PingHandler)
	http.HandleFunc(status.PingPathDW, status.PingHandler)
	http.HandleFunc(status.BuildInfoPath, status.BuildInfoHandler)
	http.HandleFunc(status.BuildInfoPathDW, status.BuildInfoHandler)
	http.HandleFunc("/__gtg", httpHandlers.goodToGo)

	http.Handle("/", r)

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Unable to start server: %v\n", err)
	}
}

func router(hh httpHandlers) http.Handler {
	servicesRouter := mux.NewRouter()

	//TODO dont know how to do gtg
	//gtgChecker := make([]gtg.StatusChecker, 0)

	servicesRouter.HandleFunc("/__health", v1a.Handler("ConceptIngester Healthchecks",
		"Checks for accessing writer", hh.healthCheck()))

	servicesRouter.HandleFunc("/__gtg", hh.goodToGo)

	//TODO check writers /__health endpoint?
	//gtgChecker = append(gtgChecker, func() gtg.Status {
	//	if err := eng.Check(); err != nil {
	//		return gtg.Status{GoodToGo: false, Message: err.Error()}
	//	}
	//
	//	return gtg.Status{GoodToGo: true}
	//})

	var monitoringRouter http.Handler = servicesRouter
	monitoringRouter = httphandlers.TransactionAwareRequestLoggingHandler(log.StandardLogger(), monitoringRouter)
	monitoringRouter = httphandlers.HTTPMetricsHandler(metrics.DefaultRegistry, monitoringRouter)

	return monitoringRouter
}
type httpConfigurations struct {
	baseUrlMap map[string]string
	client http.Client
}

func (httpConf httpConfigurations) readMessage(msg queueConsumer.Message) {
	var ingestionType string
	var uuid string
	for k, v := range msg.Headers {
		if k == "Message-Type" {
			ingestionType = v
		}
		if k == "Message-Id" {
			uuid = v
		}
 	}
	_, err, writerUrl := sendToWriter(ingestionType, strings.NewReader(msg.Body), uuid, httpConf.baseUrlMap, httpConf.client)

	if err == nil {
		//TODO lots of logs if INFO
		log.Debugf("Successfully written msg: %v to writer: %v\n", msg, writerUrl)
	} else {
		log.Errorf("Error processing msg: %s\n", msg)
	}
}

func sendToWriter(ingestionType string, msgBody *strings.Reader, uuid string, urlMap map[string]string, client http.Client) (resp *http.Response, err error, writerUrl string) {
	writerUrl = urlMap[ingestionType]

	reqUrl := writerUrl + "/" + uuid

	request, err := http.NewRequest("PUT", reqUrl, msgBody)
	request.ContentLength = -1
	resp, err = client.Do(request)

	//TODO cant compare ints?
	if strings.Contains(resp.Status,"200 OK") {
		return resp, err, writerUrl
	}
	//TODO Both log and error?
	message := "Concept not written! Status code was " + strconv.Itoa(resp.StatusCode)
	log.Error(message)
	err = errors.New(message)
	return resp, err, writerUrl
}