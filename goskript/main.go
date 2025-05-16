package main

import (
	"encoding/json"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"log"
	"os"
	"strconv"
	"strings"
)

type ActValuePayload struct {
	Status     string `json:"state"`
	Brightness int    `json:"brightness"`
}

var (
	statusMap = make([]ActValuePayload, 8)
	received  = 0
)

func main() {
	broker := os.Getenv("MQTT_BROKER")
	username := os.Getenv("MQTT_USER")
	clientID := os.Getenv("CLIENT_ID")
	password := os.Getenv("MQTT_PASSWORD")

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetUsername(username).
		SetPassword(password)

	opts.OnConnect = func(client mqtt.Client) {
		fmt.Println("Connected to MQTT Broker")
	}
	opts.OnConnectionLost = func(client mqtt.Client, err error) {
		fmt.Printf("Connection lost: %v\n", err)
	}

	mqttClient := mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Ошибка подключения к MQTT брокеру: %v", token.Error())
	}
	defer mqttClient.Disconnect(250)

	if token := mqttClient.Subscribe("virtual/+/status", 0, handleStatus(mqttClient)); token.Wait() && token.Error() != nil {
		log.Fatalf("Ошибка подписки: %v", token.Error())
	}

	log.Println("Ожидание сообщений в топике 'virtual/+/status'...")
	select {}
}

func handleStatus(client mqtt.Client) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		parts := strings.Split(msg.Topic(), "/")
		if len(parts) < 3 {
			return
		}
		id, err := strconv.Atoi(parts[1])
		if err != nil || id < 1 || id > 8 {
			return
		}

		var payload ActValuePayload
		if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
			log.Printf("JSON error: %v", err)
			return
		}

		statusMap[id-1] = payload
		received++

		if received == 8 {
			offIndexes := []int{}
			for i, v := range statusMap {
				if v.Status == "off" {
					offIndexes = append(offIndexes, i)
				}
			}
			processOffIndexes(client, offIndexes)
			received = 0
		}
	}
}
func processOffIndexes(client mqtt.Client, off []int) {
	if len(off) == 0 {
		return
	}
	const maxDist = 3
	brightnessMap := make([]int, 8)

	for i := 0; i < 8; i++ {
		minDist := maxDist + 1
		for _, offIdx := range off {
			dist := abs(i - offIdx)
			if dist <= maxDist && dist < minDist {
				minDist = dist
			}
		}

		if minDist == 0 {
			brightnessMap[i] = 0
		} else if minDist == 1 {
			brightnessMap[i] = 128
		} else if minDist == 2 {
			brightnessMap[i] = 190
		} else if minDist == 3 {
			brightnessMap[i] = 255
		} else {
			brightnessMap[i] = statusMap[i].Brightness // сохранить как есть
		}
	}

	for i, newBright := range brightnessMap {
		if statusMap[i].Status == "off" {
			continue
		}
		if newBright != statusMap[i].Brightness {
			topic := fmt.Sprintf("virtual/%d/brightness/set", i+1)
			payload := strconv.Itoa(newBright)
			client.Publish(topic, 0, false, payload)
			log.Printf("→ %s = %s", topic, payload)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
