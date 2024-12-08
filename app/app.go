package app

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	pb "go-meshtastic/generated/github.com/meshtastic/go/generated"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	proto "google.golang.org/protobuf/proto"

	influxdb "github.com/influxdata/influxdb1-client/v2"
	"github.com/spf13/viper"
)

var dataChan = make(chan allData)
var influxChan = make(chan sInfluxPoint)

var userCache = make(map[uint32]pb.User)

type allData struct {
	Mqtt          []byte
	Se            pb.ServiceEnvelope
	Data          pb.Data
	Tenv          pb.Telemetry_EnvironmentMetrics //Unused??
	Env           *pb.EnvironmentMetrics
	Influx        sInfluxPoint
	User          pb.User
	DeviceMetrics *pb.DeviceMetrics
}

type sInfluxPoint struct {
	Tags   map[string]string
	Fields map[string]interface{}
	Name   string
}

func celsiusToFahrenheit(celsius float32) float32 {
	return (celsius * 9 / 5) + 32
}

var messageHandler MQTT.MessageHandler = func(client MQTT.Client, msg MQTT.Message) {
	thisAllData := allData{}
	thisAllData.Mqtt = msg.Payload()

	// Unmarshal service envelope and save it to the allData struct.
	var serviceEnvelope pb.ServiceEnvelope
	if err := proto.Unmarshal(msg.Payload(), &serviceEnvelope); err != nil {
		log.Printf("Failed to decode protobuf Service Envelope message: %v", err)
		return
	}
	//fmt.Printf("Received message on topic %s: %+v\n", msg.Topic(), serviceEnvelope)
	thisAllData.Se = serviceEnvelope

	// Fill in the user data from cache.
	thisAllData.User = userCache[serviceEnvelope.Packet.From]

	// Decode the encrypted data and save it to the allData struct.
	nonce := generateNonce(serviceEnvelope.Packet.Id, serviceEnvelope.Packet.From)
	key, err := generateKey("1PG7OiApB1nwvP+rz05pAQ==") // This looks like a secret but it's the well know default key.
	if err != nil {
		log.Fatal(err)
	}
	//TODO Only decode if the ServiceEnvelope.packet tells us it's encrypted.
	decodedMessage, err := decode(key, serviceEnvelope.Packet.GetEncrypted(), nonce)
	if err != nil {
		log.Print(err)
		// Without data there's nothing more we can do.
		// Send what we have along.
		dataChan <- thisAllData
		return
	} else {
		thisAllData.Data = decodedMessage
	}

	// Handle the data layer.
	switch thisAllData.Data.Portnum {
	case pb.PortNum_TELEMETRY_APP:
		//fmt.Printf("Received telemetry from mqtt: %+v\n", thisAllData.Data)
		telemetryHandler(&thisAllData)
	case pb.PortNum_POSITION_APP:
		//fmt.Printf("Received position: %+v\n", data)
		fmt.Print("Received position from mqtt\n")
	case pb.PortNum_NODEINFO_APP:
		// We want to observe the nodeinfos to add metadata for human readable names (From the user payload in nodeinfo)
		nodeinfoHandler(&thisAllData)
	}

	// We should now have a full thisAllData.
	dataChan <- thisAllData
	//fmt.Printf("PortNum: %v\n", decodedMessage.Portnum)

}

func generateKey(key string) ([]byte, error) {
	// Pad the key with '=' characters to ensure it's a valid base64 string
	padding := (4 - len(key)%4) % 4
	paddedKey := key + strings.Repeat("=", padding)

	// Replace '-' with '+' and '_' with '/'
	replacedKey := strings.ReplaceAll(paddedKey, "-", "+")
	replacedKey = strings.ReplaceAll(replacedKey, "_", "/")

	// Decode the base64-encoded key
	return base64.StdEncoding.DecodeString(replacedKey)
}

func generateNonce(packetId uint32, node uint32) []byte {
	packetNonce := make([]byte, 8)
	nodeNonce := make([]byte, 8)

	binary.LittleEndian.PutUint32(packetNonce, packetId)
	binary.LittleEndian.PutUint32(nodeNonce, node)

	return append(packetNonce, nodeNonce...)
}

func decode(encryptionKey []byte, encryptedData []byte, nonce []byte) (pb.Data, error) {
	var message pb.Data

	ciphertext := encryptedData

	//fmt.Printf("Length of encryption key: %d\n", len(encryptionKey))
	//fmt.Printf("Encryption key: %x\n", encryptionKey)

	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return message, err
	}
	stream := cipher.NewCTR(block, nonce)
	plaintext := make([]byte, len(ciphertext))
	stream.XORKeyStream(plaintext, ciphertext)

	err = proto.Unmarshal(plaintext, &message)
	if err != nil {
		fmt.Printf("Failed to decode protobuf Data message.  This could be because we don't know the key.: %v\n", err)
	}
	return message, err
}

func telemetryHandler(thisAllData *allData) {
	var telemetry pb.Telemetry
	data := thisAllData.Data
	err := proto.Unmarshal(data.Payload, &telemetry)
	if err != nil {
		fmt.Printf("Failed to decode protobuf Telemetry message: %v\n", err)
	}
	switch telemetry.Variant.(type) {
	case *pb.Telemetry_EnvironmentMetrics:
		thisAllData.Env = telemetry.GetEnvironmentMetrics()
	case *pb.Telemetry_DeviceMetrics:
		fmt.Printf("DeviceMetrics: %+v\n", telemetry.GetDeviceMetrics())
		thisAllData.DeviceMetrics = telemetry.GetDeviceMetrics()
	}
	//fmt.Printf("Decoded telemetry: %+v\n", telemetry)
}

func addEnvironmentMetrics(thisAllData *allData) {

	fields := thisAllData.Influx.Fields
	metrics := thisAllData.Env

	if metrics == nil {
		return
	}

	if metrics.Temperature != nil {
		fields["temperature"] = celsiusToFahrenheit(*metrics.Temperature)
	}
	if metrics.RelativeHumidity != nil {
		fields["humidity"] = *metrics.RelativeHumidity
	}
	if metrics.BarometricPressure != nil {
		fields["pressure"] = *metrics.BarometricPressure
	}
	if metrics.Iaq != nil {
		fields["air_quality"] = *metrics.Iaq
	}

}

func addDataMetrics(thisAllData *allData) {
	fields := thisAllData.Influx.Fields
	tags := thisAllData.Influx.Tags
	data := thisAllData.Data

	tags["Portnum"] = fmt.Sprintf("%d", data.Portnum)
	fields["Portnum"] = data.Portnum
	//fields["Payload"] = data.Payload
	tags["WantResponse"] = fmt.Sprintf("%t", data.WantResponse)
	tags["Dest"] = fmt.Sprintf("%x", data.Dest)
	tags["Source"] = fmt.Sprintf("%x", data.Source)
	tags["RequestId"] = fmt.Sprintf("%d", data.RequestId)
	tags["ReplyId"] = fmt.Sprintf("%d", data.ReplyId)
	tags["Emoji"] = fmt.Sprintf("%d", data.Emoji)
	tags["Bitfield"] = fmt.Sprintf("%x", data.Bitfield)

}

func addUserMetrics(thisAllData *allData) {
	tags := thisAllData.Influx.Tags
	user := thisAllData.User
	tags["LongName"] = user.LongName
	tags["ShortName"] = user.ShortName
}

func addDeviceMetrics(thisAllData *allData) {
	metrics := thisAllData.DeviceMetrics
	fields := thisAllData.Influx.Fields

	fields["BatteryLevel"] = *metrics.BatteryLevel
	fields["Voltage"] = *metrics.Voltage
	fields["ChannelUtilization"] = *metrics.ChannelUtilization
	fields["AirUtilTx"] = *metrics.AirUtilTx
	fields["UptimeSeconds"] = *metrics.UptimeSeconds
}

func processServiceEnvelopePacket(serviceEnvelope pb.ServiceEnvelope) sInfluxPoint {
	// Starting with this func, our goal is to flatten out the structure into a single influx point, tags and fields.
	// This is the first step in building an influx point so create the instance of the struct.
	var influxPoint sInfluxPoint
	// ServiceEnvelope is the mqtt container for meshpacket along with some other stuff.
	// Mostly we care about the contents of the packet field.

	influxPoint.Tags = map[string]string{
		"ChannelId": serviceEnvelope.ChannelId,
		"GatewayId": serviceEnvelope.GatewayId,
		"From":      fmt.Sprintf("%x", serviceEnvelope.Packet.From),
		"To":        fmt.Sprintf("%x", serviceEnvelope.Packet.To),
		"Channel":   fmt.Sprintf("%d", serviceEnvelope.Packet.Channel),
		"Id":        fmt.Sprintf("%x", serviceEnvelope.Packet.Id),
		"HopLimit":  fmt.Sprintf("%d", serviceEnvelope.Packet.HopLimit),
		"WantAck":   fmt.Sprintf("%t", serviceEnvelope.Packet.WantAck),
		// Skipping priority
		// Skipping Delayed, it's depreciated.
		"ViaMqtt":     fmt.Sprintf("%t", serviceEnvelope.Packet.ViaMqtt),
		"PublicKey":   fmt.Sprintf("%x", serviceEnvelope.Packet.PublicKey),
		"PkiEncypted": fmt.Sprintf("%t", serviceEnvelope.Packet.PkiEncrypted),
	}
	influxPoint.Fields = map[string]interface{}{
		"RxTime":         serviceEnvelope.Packet.RxTime,
		"RxSnr":          serviceEnvelope.Packet.RxSnr,
		"RxRssi":         serviceEnvelope.Packet.RxRssi,
		"HopStart":       serviceEnvelope.Packet.HopStart,
		"PayloadVariant": serviceEnvelope.Packet.PayloadVariant,
	}
	influxPoint.Name = "MeshtasticMessage"

	return influxPoint
}

func nodeinfoHandler(thisAllData *allData) {
	var nodeinfo pb.User
	data := thisAllData.Data
	fmt.Printf("incoming data: %+v\n", data)
	err := proto.Unmarshal(data.Payload, &nodeinfo)
	if err != nil {
		fmt.Printf("Failed to decode protobuf NodeInfo message: %v\n", err)
		return
	}
	// Only really care about the User payload for now.
	thisAllData.User = nodeinfo

	// Update the user cache.
	fmt.Printf("User: %+v\n", nodeinfo)
	userCache[thisAllData.Se.Packet.From] = nodeinfo

	// Save the user cache to disk
	cacheFile, err := os.Create(viper.GetString("userinfo-cache"))
	if err != nil {
		log.Printf("Failed to create cache file: %v", err)
		return
	}
	defer cacheFile.Close()

	encoder := json.NewEncoder(cacheFile)
	if err := encoder.Encode(userCache); err != nil {
		log.Printf("Failed to encode user cache to JSON: %v", err)
	}

}

func influxPublisher(influxChan chan sInfluxPoint) {
	client, err := influxdb.NewHTTPClient(influxdb.HTTPConfig{
		Addr: viper.GetString("influx-uri"),
	})
	if err != nil {
		log.Printf("Error creating InfluxDB client: %v", err)
		return
	}
	defer client.Close()

	bp, err := influxdb.NewBatchPoints(influxdb.BatchPointsConfig{
		Database:  "msh",
		Precision: "s",
	})
	if err != nil {
		log.Printf("Error creating batch points: %v", err)
		return
	}

	for {
		select {
		case point := <-influxChan:
			pt, err := influxdb.NewPoint(point.Name, point.Tags, point.Fields, time.Now())
			if err != nil {
				log.Printf("Error creating new point: %v", err)
				return
			}
			bp.AddPoint(pt)

			if err := client.Write(bp); err != nil {
				log.Printf("Error writing batch points to InfluxDB: %v", err)
			}
		}
	}
}

func processMessages(dataChan chan allData) {
	for {
		select {
		case thisAllData := <-dataChan:

			//fmt.Printf("thisAllData: %+v\n", thisAllData)

			// Generate the Influx point for the ServiceEnvelope.Packet
			thisAllData.Influx = processServiceEnvelopePacket(thisAllData.Se)

			// Add the user data.
			addUserMetrics(&thisAllData)

			// Add influx data from the Data layer.
			addDataMetrics(&thisAllData)

			Data := thisAllData.Data
			switch Data.Portnum {
			case pb.PortNum_TELEMETRY_APP:
				//fmt.Printf("Received telemetry from chan: %+v\n", Data)
				if thisAllData.Env != nil {
					addEnvironmentMetrics(&thisAllData)
				}

				if thisAllData.DeviceMetrics != nil {
					addDeviceMetrics(&thisAllData)
				}

			case pb.PortNum_POSITION_APP:
				//fmt.Printf("Received position: %+v\n", data)
				fmt.Print("Received position\n")
			}

			// Pretty print the influx point
			fmt.Printf("Influx Point: Name=%s, Tags=%+v, Fields=%+v\n", thisAllData.Influx.Name, thisAllData.Influx.Tags, thisAllData.Influx.Fields)

			// Send the Influx point to the InfluxDB publisher.
			influxChan <- thisAllData.Influx
		}
	}
}

func Run() {
	// Viper
	mqttURI := viper.GetString("mqtt-uri")
	mqttUsername := viper.GetString("mqtt-username")
	mqttPassword := viper.GetString("mqtt-password")
	influxURI := viper.GetString("influx-uri")
	userinfoCache := viper.GetString("userinfo-cache")

	fmt.Printf("MQTT URI: %s\n", mqttURI)
	fmt.Printf("MQTT Username: %s\n", mqttUsername)
	fmt.Printf("Influx URI: %s\n", influxURI)
	fmt.Printf("Userinfo Cache: %s\n", userinfoCache)

	// Load the user cache from disk
	cacheFile, err := os.Open(viper.GetString("userinfo-cache"))
	if err != nil {
		log.Printf("Failed to open cache file: %v", err)
	} else {
		defer cacheFile.Close()
		decoder := json.NewDecoder(cacheFile)
		if err := decoder.Decode(&userCache); err != nil {
			log.Printf("Failed to decode user cache from JSON: %v", err)
		} else {
			log.Println("Loaded user cache:")
			for _, user := range userCache {
				log.Printf("User LongName: %s", user.LongName)
			}
		}
	}

	// Setup MQTT client
	opts := MQTT.NewClientOptions().AddBroker(mqttURI)
	opts.SetUsername(mqttUsername)
	opts.SetPassword(mqttPassword)
	opts.SetClientID("go_mqtt_client")
	opts.SetDefaultPublishHandler(messageHandler)

	client := MQTT.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Error connecting to broker: %v", token.Error())
	}

	if token := client.Subscribe("msh/#", 1, nil); token.Wait() && token.Error() != nil {
		log.Fatalf("Error subscribing to topic: %v", token.Error())
	}

	fmt.Println("Connected and subscribed to topic. Waiting for messages...")

	// Start other go routines.
	go influxPublisher(influxChan)
	go processMessages(dataChan)

	select {}
}
