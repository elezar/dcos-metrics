// Copyright 2016 Mesosphere, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Shopify/sarama"
)

var (
	brokersFlag = StringEnvFlag("kafka-brokers", "",
		"The Kafka brokers to connect to, as a comma separated list. (overrides -kafka-framework)")
	frameworkFlag = StringEnvFlag("kafka-framework", "kafka",
		"The Kafka framework to query for brokers.")
	flushPeriodFlag = IntEnvFlag("kafka-flush-ms", 5000,
		"Number of milliseconds to wait between output flushes")
	snappyCompressionFlag = BoolEnvFlag("kafka-compress-snappy", true,
		"Enables Snappy compression on outgoing Kafka data")
	requireAllAcksFlag = BoolEnvFlag("kafka-require-all-acks", false,
		"Requires that outgoing data be committed by all Kafka replicas (true) "+
			"rather than committed by just one replica (false)")
	kafkaVerboseFlag = BoolEnvFlag("kafka-verbose", false,
		"Enable extra logging in the underlying Kafka client.")
)

// KafkaMessage ...
type KafkaMessage struct {
	Topic string
	Data  []byte
}

// RunKafkaProducer creates and runs a Kafka Producer which sends records passed to the provided channel.
// This function should be run as a gofunc.
func RunKafkaProducer(messages <-chan KafkaMessage, stats chan<- StatsEvent) {
	if *kafkaVerboseFlag {
		sarama.Logger = log.New(os.Stdout, "[sarama] ", log.LstdFlags)
	}

	for {
		producer, err := kafkaProducer(stats)
		if err != nil {
			stats <- MakeEvent(KafkaConnectionFailed)
			log.Println("Failed to open Kafka producer:", err)
			// reuse flush period as the retry delay:
			log.Printf("Waiting for %dms\n", *flushPeriodFlag)
			time.Sleep(time.Duration(*flushPeriodFlag) * time.Millisecond)
			continue
		}
		stats <- MakeEvent(KafkaSessionOpened)
		defer func() {
			stats <- MakeEvent(KafkaSessionClosed)
			if err := producer.Close(); err != nil {
				log.Println("Failed to shut down producer cleanly:", err)
			}
		}()
		for {
			message := <-messages
			producer.Input() <- &sarama.ProducerMessage{
				Topic: message.Topic,
				Value: sarama.ByteEncoder(message.Data),
			}
			stats <- MakeEventSuff(KafkaMessageSent, message.Topic)
		}
	}
}

// ---

func kafkaProducer(stats chan<- StatsEvent) (kafkaProducer sarama.AsyncProducer, err error) {
	var brokers []string
	if len(*brokersFlag) != 0 {
		brokers = strings.Split(*brokersFlag, ",")
		if len(brokers) == 0 {
			log.Fatal("-kafka-brokers must be non-empty.")
		}
	} else if len(*frameworkFlag) != 0 {
		foundBrokers, err := lookupBrokers(*frameworkFlag)
		if err != nil {
			stats <- MakeEventSuff(KafkaLookupFailed, *frameworkFlag)
			return nil, fmt.Errorf("Broker lookup against framework %s failed: %s", *frameworkFlag, err)
		}
		brokers = append(brokers, foundBrokers...)
	} else {
		flag.Usage()
		log.Fatal("Either -kafka-framework or -kafka-brokers must be specified, or -kafka must be 'false'.")
	}
	log.Println("Kafka brokers:", strings.Join(brokers, ", "))

	kafkaProducer, err = newAsyncProducer(brokers)
	if err != nil {
		return nil, fmt.Errorf("Producer creation against brokers %+v failed: %s", brokers, err)
	}
	return kafkaProducer, nil
}

func newAsyncProducer(brokerList []string) (producer sarama.AsyncProducer, err error) {
	// For the access log, we are looking for AP semantics, with high throughput.
	// By creating batches of compressed messages, we reduce network I/O at a cost of more latency.
	config := sarama.NewConfig()
	if *requireAllAcksFlag {
		config.Producer.RequiredAcks = sarama.WaitForAll
	} else {
		config.Producer.RequiredAcks = sarama.WaitForLocal
	}
	if *snappyCompressionFlag {
		config.Producer.Compression = sarama.CompressionSnappy
	} else {
		config.Producer.Compression = sarama.CompressionNone
	}
	config.Producer.Flush.Frequency = time.Duration(*flushPeriodFlag) * time.Millisecond

	producer, err = sarama.NewAsyncProducer(brokerList, config)
	if err != nil {
		return nil, err
	}
	go func() {
		for err := range producer.Errors() {
			log.Println("Failed to write metrics to Kafka:", err)
		}
	}()
	return producer, nil
}

// ---

// Returns a list of broker endpoints, each of the form "host:port"
func lookupBrokers(framework string) (brokers []string, err error) {
	schedulerEndpoint, err := connectionEndpoint(framework)
	if err != nil {
		return nil, err
	}
	body, err := HTTPGet(schedulerEndpoint)
	if err != nil {
		return nil, err
	}
	return extractBrokers(body)
}

func connectionEndpoint(framework string) (endpoint string, err error) {
	// Perform SRV lookup to get scheduler's port number:
	// "_<framework>._tcp.marathon.mesos."
	_, addrs, err := net.LookupSRV(framework, "tcp", "marathon.mesos")
	if err != nil {
		return "", err
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("Framework '%s' not found", framework)
	}
	url := fmt.Sprintf("http://%s.mesos:%d/v1/connection", framework, addrs[0].Port)
	log.Println("Fetching broker list from Kafka Framework at:", url)
	return url, nil
}

func extractBrokers(body []byte) (brokers []string, err error) {
	var jsonData map[string]interface{}
	if err = json.Unmarshal(body, &jsonData); err != nil {
		return nil, err
	}
	// expect "dns" entry containing a list of strings
	jsonBrokers := jsonData["dns"].([]interface{})
	brokers = make([]string, len(jsonBrokers))
	for i, jsonDNSEntry := range jsonBrokers {
		brokers[i] = jsonDNSEntry.(string)
	}
	return brokers, nil
}