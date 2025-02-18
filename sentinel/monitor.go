package sentinel

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/mdevilliers/redishappy/services/logger"
	"github.com/mdevilliers/redishappy/services/redis"
	"github.com/mdevilliers/redishappy/types"
)

type Monitor struct {
	pubSubClient    *redis.PubSubClient
	client          *redis.SentinelClient
	channel         chan redis.RedisPubSubReply
	manager         Manager
	sentinel        types.Sentinel
	redisConnection redis.RedisConnection
}

func NewMonitor(sentinel types.Sentinel, manager Manager, redisConnection redis.RedisConnection, tcpKeepAlive int) (*Monitor, error) {

	uri := sentinel.GetLocation()

	channel := make(chan redis.RedisPubSubReply)
	pubSubClient, err := redis.NewPubSubClient(uri, channel, redisConnection, tcpKeepAlive)

	if err != nil {
		return nil, err
	}

	client, err := redis.NewSentinelClient(sentinel, redisConnection, tcpKeepAlive)

	if err != nil {
		return nil, err
	}

	monitor := &Monitor{pubSubClient: pubSubClient,
		client:          client,
		channel:         channel,
		manager:         manager,
		sentinel:        sentinel,
		redisConnection: redisConnection}
	return monitor, nil
}

func (m *Monitor) StartMonitoringMasterEvents(switchmasterchannel chan types.MasterSwitchedEvent, connectionChannel chan types.ConnectionEvent) error {

	key := "+switch-master"
	err := m.pubSubClient.Start(key)

	if err != nil {
		logger.Error.Printf("Error StartMonitoringMasterEvents %s sentinel@%s", err.Error(), m.sentinel.GetLocation())
		m.pubSubClient.Close()
		return err
	}

	go m.loop(switchmasterchannel, connectionChannel)

	return nil
}

func (m *Monitor) loop(switchmasterchannel chan types.MasterSwitchedEvent, connectionChannel chan types.ConnectionEvent) {
L:
	for {
		select {
		case message := <-m.channel:
			shutdown := dealWithSentinelMessage(message, switchmasterchannel, connectionChannel)
			if shutdown {
				m.shutDownMonitor()
				break L
			}

		case <-time.After(MonitorPingInterval):

			err := m.client.Ping()
			if err != nil {
				logger.Info.Printf("Error pinging client : %s, response : %s", m.sentinel.GetLocation(), err.Error())
				m.shutDownMonitor()
				break L
			}

			m.manager.Notify(&SentinelPing{Sentinel: m.sentinel})

			knownClusters, err := m.client.FindKnownClusters()

			if err != nil {
				logger.Info.Printf("Error discovering clusters : %s, response : %s", m.sentinel.GetLocation(), err.Error())
				m.shutDownMonitor()
				break L
			}

			m.manager.Notify(&SentinelClustersMonitoredUpdate{Sentinel: m.sentinel, Clusters: knownClusters})

			for _, clustername := range knownClusters {

				sentinels, err := m.client.FindConnectedSentinels(clustername)

				if err != nil {
					logger.Info.Printf("Error finding connected sentinels : %s, response : %s", m.sentinel.GetLocation(), err.Error())
					m.shutDownMonitor()
					break L
				}

				for _, connectedsentinel := range sentinels {
					logger.Info.Printf("Adding sentinel : %s", connectedsentinel.GetLocation())
					m.manager.Notify(&SentinelAdded{Sentinel: connectedsentinel})
				}
			}
		}
	}
}

func (m *Monitor) shutDownMonitor() {
	logger.Info.Printf("Shutting down monitor %s", m.sentinel.GetLocation())
	m.manager.Notify(&SentinelLost{Sentinel: m.sentinel})
	m.client.Close()
	m.pubSubClient.Close()
}

func dealWithSentinelMessage(message redis.RedisPubSubReply, switchmasterchannel chan types.MasterSwitchedEvent, connectionChannel chan types.ConnectionEvent) bool {

	if message.Err() != nil {
		logger.Info.Printf("Subscription Message : %s : Error %s", message.Channel(), message.Err())
		return true
	}

	logger.Info.Printf("Subscription Message : Channel : %s : %s", message.Channel(), message.Message())

	if message.MessageType() == redis.Message {

		event, err := parseSwitchMasterMessage(message.Message())

		if err != nil {
			logger.Error.Printf("Subscription Message : Channel %s : Error parsing message %s %s", message.Channel(), message.Message(), err.Error())
			return true
		} else {
			switchmasterchannel <- event
		}
	}

	// If we've sucessfully subscribed, let the manager know, so it can force a topology resync.
	if message.MessageType() == redis.Confirmation {
		if message.Message() == "1" {
			// We should expect a value of 1 here (appears to be a subscription counter)
			logger.Trace.Println("Subscription Message : Firing a ConnectionEvent")
			connectionChannel <- types.ConnectionEvent{Connected: true}
		} else {
			// Something odd has occurred. We're not guarantted to be subscribed.
			return true
		}
	}

	return false
}

func parseSwitchMasterMessage(message string) (types.MasterSwitchedEvent, error) {

	bits := strings.Split(message, " ")
	if len(bits) != 5 {
		return types.MasterSwitchedEvent{}, errors.New("Invalid message recieved")
	}

	oldmasterport, err := strconv.Atoi(bits[2])

	if err != nil {
		return types.MasterSwitchedEvent{}, err
	}

	newmasterport, err := strconv.Atoi(bits[4])

	if err != nil {
		return types.MasterSwitchedEvent{}, err
	}

	return types.MasterSwitchedEvent{Name: bits[0], OldMasterIp: bits[1], OldMasterPort: oldmasterport, NewMasterIp: bits[3], NewMasterPort: newmasterport}, nil
}
