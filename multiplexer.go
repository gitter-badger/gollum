package main

import (
	"fmt"
	"github.com/trivago/gollum/consumer"
	"github.com/trivago/gollum/shared"
	"os"
	"os/signal"
	"sync"
)

type multiplexer struct {
	consumers       []shared.Consumer
	producers       []shared.Producer
	consumerThreads *sync.WaitGroup
	producerThreads *sync.WaitGroup
	stream          map[shared.MessageStreamID][]shared.Producer
}

// Create a new multiplexer based on a given config file.
func newMultiplexer(configFile string) multiplexer {
	conf, err := shared.ReadConfig(configFile)
	if err != nil {
		fmt.Printf("Error: %s", err.Error())
		os.Exit(-1)
	}

	// Configure the multiplexer, create a byte pool and assign it to the log

	logConsumer := consumer.Log{}
	logConsumer.Configure(shared.PluginConfig{})

	plex := multiplexer{
		stream:          make(map[shared.MessageStreamID][]shared.Producer),
		consumerThreads: new(sync.WaitGroup),
		producerThreads: new(sync.WaitGroup),
		consumers:       []shared.Consumer{logConsumer},
	}

	// Initialize the plugins based on the config

	for className, instanceConfigs := range conf.Settings {
		for _, config := range instanceConfigs {
			if !config.Enable {
				continue // ### continue, disabled ###
			}

			// Try to instantiate and configure the plugin

			obj, err := shared.RuntimeType.New(className)
			if err != nil {
				shared.Log.Error.Panic(err.Error())
			}

			plugin, isPlugin := obj.(shared.Plugin)
			if !isPlugin {
				shared.Log.Error.Panic(className, " is no plugin.")
				continue // ### continue ###
			}

			err = plugin.Configure(config)
			if err != nil {
				shared.Log.Error.Print("Failed to configure plugin ", className, ": ", err)
				continue // ### continue ###
			}

			// Register consumer plugins

			if consumer, isConsumer := obj.(shared.Consumer); isConsumer {
				plex.consumers = append(plex.consumers, consumer)
			}

			// Register producer plugins

			if producer, isProducer := obj.(shared.Producer); isProducer {
				plex.producers = append(plex.producers, producer)

				for _, stream := range config.Stream {
					streamID := shared.GetStreamID(stream)
					streamMap, mappingExists := plex.stream[streamID]

					if !mappingExists {
						plex.stream[streamID] = []shared.Producer{producer}
					} else {
						plex.stream[streamID] = append(streamMap, producer)
					}
				}
			}
		}
	}

	return plex
}

// sendMessage sends a message to all producers listening to a given stream.
// This method blocks as long as a producer message queue is full.
// You can pass false to the enqueue parameter to ignore inactive plugins (i.e.
// useful during shutdown)
func (plex multiplexer) sendMessage(message shared.Message, streamID shared.MessageStreamID, enqueue bool) {
	msgClone := message.CloneAndPin(streamID)
	for _, producer := range plex.stream[streamID] {
		if (producer.IsActive() || enqueue) && producer.Accepts(msgClone) {
			producer.Messages() <- msgClone
		}
	}
}

// broadcastMessage sends a message to all streams the message has been
// addressed to.
// This method blocks if sendMessage blocks.
func (plex multiplexer) broadcastMessage(message shared.Message, enqueue bool) {
	// Send to wildcard stream producers if not purely internal
	if !message.IsInternal() {
		plex.sendMessage(message, shared.WildcardStreamID, enqueue)
	}
	// Send to specific stream producers
	for _, streamID := range message.Streams {
		plex.sendMessage(message, streamID, enqueue)
	}
}

// Shutdown all consumers and producers in a clean way.
// The internal log is flushed after the consumers have been shut down so that
// consumer related messages are still in the log.
// Producers are flushed after flushing the log, so producer related shutdown
// messages will be posted to stdout
func (plex *multiplexer) shutdown() {
	shared.Log.Note.Print("Filthy little hobbites. They stole it from us. (shutdown)")

	// Send shutdown to consumers

	for _, consumer := range plex.consumers {
		consumer.Control() <- shared.ConsumerControlStop
	}
	plex.consumerThreads.Wait()

	// Make sure all remaining messages are flushed

	shared.Log.Note.Print("Sending the hobbits to mount doom. (flushing)")

	for _, consumer := range plex.consumers {
	flushing:
		for {
			select {
			case message := <-consumer.Messages():
				plex.broadcastMessage(message, false)
			default:
				break flushing
			}
		}
	}

	// Shutdown producers

	for _, producer := range plex.producers {
		producer.Control() <- shared.ProducerControlStop
	}
	plex.producerThreads.Wait()

	// Write remaining messages to stderr

	format := shared.NewMessageFormatForward()

	for {
		select {
		case message := <-shared.Log.Messages():
			fmt.Fprintln(os.Stdout, format.ToString(message))
		default:
			return
		}
	}
}

// Run the multiplexer.
// Fetch messags from the consumers and pass them to all producers.
func (plex multiplexer) run() {
	defer plex.shutdown()

	if len(plex.consumers) == 0 {
		shared.Log.Error.Print("No consumers configured.")
		return // ### return, nothing to do ###
	}

	if len(plex.producers) == 0 {
		shared.Log.Error.Print("No producers configured.")
		return // ### return, nothing to do ###
	}

	// React on signals and setup the MessageProvider queue

	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, os.Interrupt)

	// Launch consumers and producers

	for _, producer := range plex.producers {
		go producer.Produce(plex.producerThreads)
	}

	for _, consumer := range plex.consumers {
		go consumer.Consume(plex.consumerThreads)
	}

	// Wait for at least one producer to come online

	shared.Log.Note.Print("We be nice to them, if they be nice to us. (startup)")

	// Main loop

	for {
		// Go over all consumers in round-robin fashion
		// Don't block here, too as a consumer might not contain new messages

		for _, consumer := range plex.consumers {
			select {
			default:
				// do nothing

			case <-signalChannel:
				shared.Log.Note.Print("Master betrayed us. Wicked. Tricksy, False. (signal)")
				return

			case message := <-consumer.Messages():
				plex.broadcastMessage(message, true)
			}
		}
	}
}
