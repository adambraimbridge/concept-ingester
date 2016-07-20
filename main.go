package main

import (
	"net/http"
	"os"

	"strings"

	queueConsumer "github.com/Financial-Times/message-queue-gonsumer/consumer"

	"io"
	"io/ioutil"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"net"

	"fmt"

	"errors"

	"github.com/Financial-Times/go-fthealth/v1a"
	"github.com/Financial-Times/http-handlers-go/httphandlers"
	status "github.com/Financial-Times/service-status-go/httphandlers"
	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/jawher/mow.cli"
	"github.com/rcrowley/go-metrics"
)

var httpClient = http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 128,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
	},
}
var ticker *time.Ticker

func main() {
	log.SetLevel(log.InfoLevel)
	app := cli.App("concept-ingester", "A microservice that consumes concept messages from Kafka and routes them to the appropriate writer")

	services := app.String(cli.StringOpt{
		Name:   "services-list",
		Value:  "services",
		Desc:   "writer services",
		EnvVar: "SERVICES",
	})
	port := app.String(cli.StringOpt{
		Name:   "port",
		Value:  "8080",
		Desc:   "Port to listen on",
		EnvVar: "PORT",
	})
	vulcanAddr := app.String(cli.StringOpt{
		Name:   "vulcan_addr",
		Value:  "https://vulcan-address",
		Desc:   "Vulcan address for routing requests",
		EnvVar: "VULCAN_ADDR",
	})
	consumerGroupID := app.String(cli.StringOpt{
		Name:   "consumer_group_id",
		Value:  "ConceptIngesterGroup",
		Desc:   "Kafka group id used for message consuming.",
		EnvVar: "GROUP_ID",
	})
	consumerQueue := app.String(cli.StringOpt{
		Name:   "consumer_queue_id",
		Value:  "",
		Desc:   "Sets host header",
		EnvVar: "HOST_HEADER",
	})
	consumerOffset := app.String(cli.StringOpt{
		Name:   "consumer_offset",
		Value:  "",
		Desc:   "Kafka read offset.",
		EnvVar: "OFFSET"})
	consumerAutoCommitEnable := app.Bool(cli.BoolOpt{
		Name:   "consumer_autocommit_enable",
		Value:  true,
		Desc:   "Enable autocommit for small messages.",
		EnvVar: "COMMIT_ENABLE"})
	topic := app.String(cli.StringOpt{
		Name:   "topic",
		Value:  "kafka-topic",
		Desc:   "Kafka topic subscribed to",
		EnvVar: "TOPIC"})
	throttle := app.Int(cli.IntOpt{
		Name:   "throttle",
		Value:  1000,
		Desc:   "Throttle",
		EnvVar: "THROTTLE"})

	app.Action = func() {
		consumerConfig := queueConsumer.QueueConfig{
			Addrs:                strings.Split(*vulcanAddr, ","),
			Group:                *consumerGroupID,
			Queue:                *consumerQueue,
			Topic:                *topic,
			Offset:               *consumerOffset,
			AutoCommitEnable:     *consumerAutoCommitEnable,
			ConcurrentProcessing: true,
		}

		ticker = time.NewTicker(time.Second / time.Duration(*throttle))
		writersSlice := createWritersSlice(*services, *vulcanAddr)
		httpConfigurations := httpConfigurations{baseURLSlice: writersSlice}
		consumer := queueConsumer.NewConsumer(consumerConfig, httpConfigurations.readMessage, httpClient)

		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			consumer.Start()
			wg.Done()
		}()

		go runServer(httpConfigurations.baseURLSlice, *port, *vulcanAddr, *topic)

		ch := make(chan os.Signal)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

		<-ch
		log.Println("Shutting down application...")

		consumer.Stop()
		wg.Wait()

		log.Println("Application closing")
	}
	app.Run(os.Args)
}

func createWritersSlice(services string, vulcanAddr string) []string {
	var writerSlice []string
	serviceSlice := strings.Split(services, ",")
	for _, service := range serviceSlice {
		serviceAddr := resolveServiceAddress(service, vulcanAddr)
		writerURL := serviceAddr[0] + "/__" + serviceAddr[1]
		writerSlice = append(writerSlice, writerURL)
		log.Infof("Using writer url: %s", writerURL)
	}
	return writerSlice
}
func resolveServiceAddress(writer string, vulcanAddr string) []string {
	wr := strings.Split(writer, ":")
	if len(wr) > 1 { return []string{"http://localhost:" + wr[1], wr[0]}
	}
	return []string {vulcanAddr, writer}

}

func runServer(baseURLSlice []string, port string, vulcanAddr string, topic string) {

	httpHandlers := httpHandlers{baseURLSlice, vulcanAddr, topic}

	r := router(httpHandlers)
	// The following endpoints should not be monitored or logged (varnish calls one of these every second, depending on config)
	// The top one of these build info endpoints feels more correct, but the lower one matches what we have in Dropwizard,
	// so it's what apps expect currently same as ping, the content of build-info needs more definition
	http.HandleFunc(status.PingPath, status.PingHandler)
	http.HandleFunc(status.PingPathDW, status.PingHandler)
	http.HandleFunc(status.BuildInfoPath, status.BuildInfoHandler)
	http.HandleFunc(status.BuildInfoPathDW, status.BuildInfoHandler)
	log.Infof("concept-ingester-go-app will listen on port: %s", port)

	http.Handle("/", r)

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Unable to start server: %v\n", err)
	}
}

func router(hh httpHandlers) http.Handler {
	servicesRouter := mux.NewRouter()
	servicesRouter.HandleFunc("/__health", v1a.Handler("ConceptIngester Healthchecks", "Checks for accessing writer", hh.kafkaProxyHealthCheck(), hh.writerHealthCheck()))
	servicesRouter.HandleFunc("/__gtg", hh.goodToGo)

	var monitoringRouter http.Handler = servicesRouter
	monitoringRouter = httphandlers.TransactionAwareRequestLoggingHandler(log.StandardLogger(), monitoringRouter)
	monitoringRouter = httphandlers.HTTPMetricsHandler(metrics.DefaultRegistry, monitoringRouter)

	return monitoringRouter
}

type httpConfigurations struct {
	baseURLSlice []string
	client       http.Client
}

func (httpConf httpConfigurations) readMessage(msg queueConsumer.Message) {
	<-ticker.C
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
	err := sendToWriter(ingestionType, strings.NewReader(msg.Body), uuid, httpConf.baseURLSlice)

	if err != nil {
		log.Errorf("%v", err)
	}
}

func sendToWriter(ingestionType string, msgBody io.Reader, uuid string, URLSlice []string) error {
	var writerURL string
	for _, URL := range URLSlice {
		if strings.Contains(URL, ingestionType) {
			writerURL = URL
			break
		}
	}
	if writerURL == "" {
		return errors.New("Writer url is blank for concept=" + ingestionType + "and uuid=" + uuid)
	}
	reqURL := writerURL + "/" + ingestionType + "/" + uuid

	request, err := http.NewRequest("PUT", reqURL, msgBody)
	if err != nil {
		return fmt.Errorf("Failed to create request to %v with body %v", reqURL, msgBody)
	}
	request.ContentLength = -1

	resp, reqErr := httpClient.Do(request)
	if reqErr != nil {
		return fmt.Errorf("reqURL=[%s] concept=[%s] uuid=[%s] error=[%v]", reqURL, ingestionType, uuid, reqErr)
	}

	if resp.StatusCode == http.StatusOK {
		readBody(resp)
		return nil
	}

	defer resp.Body.Close()
	errorMessage, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Errorf("Cannot read error body: [%v]", err)
	}

	return fmt.Errorf("reqURL=[%s] status=[%d] concept=[%s] uuid=[%s] error=[%v] body=[%s]", reqURL, resp.StatusCode, ingestionType, uuid, reqErr, string(errorMessage))
}

func readBody(resp *http.Response) {
	io.Copy(ioutil.Discard, resp.Body)
	resp.Body.Close()
}
