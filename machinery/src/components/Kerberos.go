package components

import (
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kerberos-io/joy4/cgo/ffmpeg"

	"github.com/kerberos-io/agent/machinery/src/capture"
	"github.com/kerberos-io/agent/machinery/src/cloud"
	"github.com/kerberos-io/agent/machinery/src/computervision"
	"github.com/kerberos-io/agent/machinery/src/log"
	"github.com/kerberos-io/agent/machinery/src/models"
	"github.com/kerberos-io/agent/machinery/src/onvif"
	routers "github.com/kerberos-io/agent/machinery/src/routers/mqtt"
	"github.com/kerberos-io/joy4/av"
	"github.com/kerberos-io/joy4/av/pubsub"
	"github.com/tevino/abool"
)

func Bootstrap(configuration *models.Configuration, communication *models.Communication) {
	log.Log.Debug("Bootstrap: started")

	// We will keep track of the Kerberos Agent up time
	// This is send to Kerberos Hub in a heartbeat.
	uptimeStart := time.Now()

	// Initiate the packet counter, this is being used to detect
	// if a camera is going blocky, or got disconnected.
	var packageCounter atomic.Value
	packageCounter.Store(int64(0))
	communication.PackageCounter = &packageCounter

	// This is used when the last packet was received (timestamp),
	// this metric is used to determine if the camera is still online/connected.
	var lastPacketTimer atomic.Value
	packageCounter.Store(int64(0))
	communication.LastPacketTimer = &lastPacketTimer

	// This is used to understand if we have a working Kerberos Hub connection
	// cloudTimestamp will be updated when successfully sending heartbeats.
	var cloudTimestamp atomic.Value
	cloudTimestamp.Store(int64(0))
	communication.CloudTimestamp = &cloudTimestamp

	communication.HandleStream = make(chan string, 1)
	communication.HandleSubStream = make(chan string, 1)
	communication.HandleUpload = make(chan string, 1)
	communication.HandleHeartBeat = make(chan string, 1)
	communication.HandleLiveSD = make(chan int64, 1)
	communication.HandleLiveHDKeepalive = make(chan string, 1)
	communication.HandleLiveHDPeers = make(chan string, 1)
	communication.IsConfiguring = abool.New()

	// Before starting the agent, we have a control goroutine, that might
	// do several checks to see if the agent is still operational.
	go ControlAgent(communication)

	// Create some global variables
	decoder := &ffmpeg.VideoDecoder{}
	subDecoder := &ffmpeg.VideoDecoder{}
	cameraSettings := &models.Camera{}

	// Run the agent and fire up all the other
	// goroutines which do image capture, motion detection, onvif, etc.

	for {
		// This will blocking until receiving a signal to be restarted, reconfigured, stopped, etc.
		status := RunAgent(configuration, communication, uptimeStart, cameraSettings, decoder, subDecoder)
		if status == "stop" {
			break
		}
		// We will re open the configuration, might have changed :O!
		OpenConfig(configuration)
	}
	log.Log.Debug("Bootstrap: finished")
}

func RunAgent(configuration *models.Configuration, communication *models.Communication, uptimeStart time.Time, cameraSettings *models.Camera, decoder *ffmpeg.VideoDecoder, subDecoder *ffmpeg.VideoDecoder) string {
	log.Log.Debug("RunAgent: started")

	config := configuration.Config

	// Currently only support H264 encoded cameras, this will change.
	// Establishing the camera connection
	log.Log.Info("RunAgent: opening RTSP stream")
	rtspUrl := config.Capture.IPCamera.RTSP
	infile, streams, err := capture.OpenRTSP(rtspUrl)

	var queue *pubsub.Queue
	var subQueue *pubsub.Queue

	var decoderMutex sync.Mutex
	var subDecoderMutex sync.Mutex

	status := "not started"

	if err == nil {

		log.Log.Info("RunAgent: opened RTSP stream")

		// We might have a secondary rtsp url, so we might need to use that.
		var subInfile av.DemuxCloser
		var subStreams []av.CodecData
		subStreamEnabled := false
		subRtspUrl := config.Capture.IPCamera.SubRTSP
		if subRtspUrl != "" && subRtspUrl != rtspUrl {
			subInfile, subStreams, err = capture.OpenRTSP(subRtspUrl)
			if err == nil {
				log.Log.Info("RunAgent: opened RTSP sub stream")
				subStreamEnabled = true
			}
		}

		// We will initialise the camera settings object
		// so we can check if the camera settings have changed, and we need
		// to reload the decoders.
		videoStream, _ := capture.GetVideoStream(streams)
		num, denum := videoStream.(av.VideoCodecData).Framerate()
		width := videoStream.(av.VideoCodecData).Width()
		height := videoStream.(av.VideoCodecData).Height()

		if cameraSettings.RTSP != rtspUrl || cameraSettings.SubRTSP != subRtspUrl || cameraSettings.Width != width || cameraSettings.Height != height || cameraSettings.Num != num || cameraSettings.Denum != denum || cameraSettings.Codec != videoStream.(av.VideoCodecData).Type() {

			if cameraSettings.Initialized {
				decoder.Close()
				if subStreamEnabled {
					subDecoder.Close()
				}
			}

			// At some routines we will need to decode the image.
			// Make sure its properly locked as we only have a single decoder.
			log.Log.Info("RunAgent: camera settings changed, reloading decoder")
			capture.GetVideoDecoder(decoder, streams)
			if subStreamEnabled {
				capture.GetVideoDecoder(subDecoder, subStreams)
			}

			cameraSettings.RTSP = rtspUrl
			cameraSettings.SubRTSP = subRtspUrl
			cameraSettings.Width = width
			cameraSettings.Height = height
			cameraSettings.Framerate = float64(num) / float64(denum)
			cameraSettings.Num = num
			cameraSettings.Denum = denum
			cameraSettings.Codec = videoStream.(av.VideoCodecData).Type()
			cameraSettings.Initialized = true
		} else {
			log.Log.Info("RunAgent: camera settings did not change, keeping decoder")
		}

		communication.Decoder = decoder
		communication.SubDecoder = subDecoder
		communication.DecoderMutex = &decoderMutex
		communication.SubDecoderMutex = &subDecoderMutex

		// Create a packet queue, which is filled by the HandleStream routing
		// and consumed by all other routines: motion, livestream, etc.
		if config.Capture.PreRecording <= 0 {
			config.Capture.PreRecording = 1
			log.Log.Warning("RunAgent: Prerecording value not found in config or invalid value! Found: " + strconv.FormatInt(config.Capture.PreRecording, 10))
		}

		// We are creating a queue to store the RTSP frames in, these frames will be
		// processed by the different consumers: motion detection, recording, etc.
		queue = pubsub.NewQueue()
		communication.Queue = queue
		queue.SetMaxGopCount(int(config.Capture.PreRecording) + 1) // GOP time frame is set to prerecording (we'll add 2 gops to leave some room).
		log.Log.Info("RunAgent: SetMaxGopCount was set with: " + strconv.Itoa(int(config.Capture.PreRecording)+1))
		queue.WriteHeader(streams)

		// We might have a substream, if so we'll create a seperate queue.
		if subStreamEnabled {
			log.Log.Info("RunAgent: Creating sub stream queue with SetMaxGopCount set to " + strconv.Itoa(int(1)))
			subQueue = pubsub.NewQueue()
			communication.SubQueue = subQueue
			subQueue.SetMaxGopCount(1)
			subQueue.WriteHeader(subStreams)
		}

		// Configure a MQTT client which helps for a bi-directional communication
		communication.HandleONVIF = make(chan models.OnvifAction, 1)
		mqttClient := routers.ConfigureMQTT(configuration, communication)

		// Handle heartbeats
		go cloud.HandleHeartBeat(configuration, communication, uptimeStart)

		// Handle the camera stream
		go capture.HandleStream(infile, queue, communication)

		// Handle the substream if enabled
		if subStreamEnabled {
			go capture.HandleSubStream(subInfile, subQueue, communication)
		}

		// Handle processing of motion
		communication.HandleMotion = make(chan models.MotionDataPartial, 1)
		if subStreamEnabled {
			motionCursor := subQueue.Latest()
			go computervision.ProcessMotion(motionCursor, configuration, communication, mqttClient, subDecoder, &subDecoderMutex)
		} else {
			motionCursor := queue.Latest()
			go computervision.ProcessMotion(motionCursor, configuration, communication, mqttClient, decoder, &decoderMutex)
		}

		// Handle livestream SD (low resolution over MQTT)
		if subStreamEnabled {
			livestreamCursor := subQueue.Latest()
			go cloud.HandleLiveStreamSD(livestreamCursor, configuration, communication, mqttClient, subDecoder, &subDecoderMutex)
		} else {
			livestreamCursor := queue.Latest()
			go cloud.HandleLiveStreamSD(livestreamCursor, configuration, communication, mqttClient, decoder, &decoderMutex)
		}

		// Handle livestream HD (high resolution over WEBRTC)
		communication.HandleLiveHDHandshake = make(chan models.SDPPayload, 1)
		if subStreamEnabled {
			livestreamHDCursor := subQueue.Latest()
			go cloud.HandleLiveStreamHD(livestreamHDCursor, configuration, communication, mqttClient, subStreams, subDecoder, &decoderMutex)
		} else {
			livestreamHDCursor := queue.Latest()
			go cloud.HandleLiveStreamHD(livestreamHDCursor, configuration, communication, mqttClient, streams, decoder, &decoderMutex)
		}

		// Handle recording, will write an mp4 to disk.
		go capture.HandleRecordStream(queue, configuration, communication, streams)

		// Handle Upload to cloud provider (Kerberos Hub, Kerberos Vault and others)
		go cloud.HandleUpload(configuration, communication)

		// Handle ONVIF actions
		go onvif.HandleONVIFActions(configuration, communication)

		// !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
		// This will go into a blocking state, once this channel is triggered
		// the agent will cleanup and restart.
		status = <-communication.HandleBootstrap

		close(communication.HandleONVIF)
		communication.HandleONVIF = nil
		close(communication.HandleLiveHDHandshake)
		communication.HandleLiveHDHandshake = nil
		close(communication.HandleMotion)
		communication.HandleMotion = nil

		// Here we are cleaning up everything!
		if configuration.Config.Offline != "true" {
			communication.HandleHeartBeat <- "stop"
			communication.HandleUpload <- "stop"
		}
		communication.HandleStream <- "stop"
		if subStreamEnabled {
			communication.HandleSubStream <- "stop"
		}
		time.Sleep(time.Second * 1)

		infile.Close()
		infile = nil
		queue.Close()
		queue = nil
		communication.Queue = nil
		if subStreamEnabled {
			subInfile.Close()
			subInfile = nil
			subQueue.Close()
			subQueue = nil
			communication.SubQueue = nil
		}

		// Disconnect MQTT
		routers.DisconnectMQTT(mqttClient, &configuration.Config)

		// Wait a few seconds to stop the decoder.
		time.Sleep(time.Second * 3)

		// Waiting for some seconds to make sure everything is properly closed.
		log.Log.Info("RunAgent: waiting 3 seconds to make sure everything is properly closed.")
		time.Sleep(time.Second * 3)
	} else {
		log.Log.Error("Something went wrong while opening RTSP: " + err.Error())
		time.Sleep(time.Second * 3)
	}

	log.Log.Debug("RunAgent: finished")

	// Clean up, force garbage collection
	runtime.GC()

	return status
}

func ControlAgent(communication *models.Communication) {
	log.Log.Debug("ControlAgent: started")
	packageCounter := communication.PackageCounter
	go func() {
		// A channel to check the camera activity
		var previousPacket int64 = 0
		var occurence = 0
		for {
			packetsR := packageCounter.Load().(int64)
			if packetsR == previousPacket {
				// If we are already reconfiguring,
				// we dont need to check if the stream is blocking.
				if !communication.IsConfiguring.IsSet() {
					occurence = occurence + 1
				}
			} else {

				occurence = 0
			}

			log.Log.Info("ControlAgent: Number of packets read " + strconv.FormatInt(packetsR, 10))

			// After 15 seconds without activity this is thrown..
			if occurence == 3 {
				log.Log.Info("Main: Restarting machinery.")
				communication.HandleBootstrap <- "restart"
				time.Sleep(2 * time.Second)
				occurence = 0
			}
			previousPacket = packageCounter.Load().(int64)

			time.Sleep(5 * time.Second)
		}
	}()
	log.Log.Debug("ControlAgent: finished")
}
