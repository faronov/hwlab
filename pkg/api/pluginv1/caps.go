package pluginv1

// ProtocolVersion is the plugin protocol version spoken by this build.
const ProtocolVersion uint32 = 1

// Process-level handshake between core and plugin executables.
const (
	// EnvSocket is the unix socket path the plugin must listen on.
	EnvSocket = "HWLAB_PLUGIN_SOCKET"
	// EnvWatchStdin tells the plugin to exit when stdin is closed (core death).
	EnvWatchStdin = "HWLAB_PLUGIN_WATCH_STDIN"
	// HandshakePrefix starts the single stdout line a plugin prints once it is
	// listening: HWLAB-PLUGIN|<protocol_version>|unix|<path>.
	HandshakePrefix = "HWLAB-PLUGIN"
)

// Well-known capability ids. The set is open: plugins may declare any id and
// the core must tolerate ids it does not know.
const (
	CapPowerMeasurement   = "PowerMeasurement"
	CapVoltageMeasurement = "VoltageMeasurement"
	CapPowerSource        = "PowerSource"

	CapDigitalCapture = "DigitalCapture"
	CapAnalogCapture  = "AnalogCapture"

	CapPacketCapture    = "PacketCapture"
	CapProtocolDecode   = "ProtocolDecode"
	CapUSBPacketCapture = "USBPacketCapture"

	CapDebugProbe       = "DebugProbe"
	CapMemoryAccess     = "MemoryAccess"
	CapFlashProgramming = "FlashProgramming"
	CapRTT              = "RTT"

	// JTAG probe transport: shifting bits through a TAP chain, enumerating
	// it and identifying devices by IDCODE. These are not CPU debug
	// capabilities; they do not imply DebugProbe, MemoryAccess or flash.
	CapJTAGTransport  = "JTAGTransport"
	CapJTAGScan       = "JTAGScan"
	CapTargetIdentify = "TargetIdentify"

	CapTriggerInput  = "TriggerInput"
	CapTriggerOutput = "TriggerOutput"

	CapLiveRawStream  = "LiveRawStream"
	CapTimedCapture   = "TimedCapture"
	CapOfflineCapture = "OfflineCapture"
	CapArtifactExport = "ArtifactExport"

	CapSCPI            = "SCPI"
	CapOscilloscope    = "Oscilloscope"
	CapSerialTransport = "SerialTransport"
	CapCANTransport    = "CANTransport"
	CapModbus          = "Modbus"
)

// Well-known event types (Event.type). Derived from the payload case when the
// plugin leaves Event.type empty.
const (
	TypePowerBatch   = "PowerBatch"
	TypeAnalogBatch  = "AnalogBatch"
	TypeDigitalBatch = "DigitalBatch"
	TypePacket       = "Packet"
	TypeLog          = "Log"
	TypeMarker       = "Marker"
	TypeDecoded      = "DecodedProtocolEvent"
	TypeTrigger      = "Trigger"
	TypeDebug        = "DebugEvent"
)

// HasCapability reports whether caps contains id.
func HasCapability(caps []*Capability, id string) bool {
	for _, c := range caps {
		if c.GetId() == id {
			return true
		}
	}
	return false
}

// EventType returns the explicit or payload-derived type of e.
func EventType(e *Event) string {
	if e.GetType() != "" {
		return e.GetType()
	}
	switch e.GetPayload().(type) {
	case *Event_Power:
		return TypePowerBatch
	case *Event_Analog:
		return TypeAnalogBatch
	case *Event_Digital:
		return TypeDigitalBatch
	case *Event_Packet:
		return TypePacket
	case *Event_Log:
		return TypeLog
	case *Event_Marker:
		return TypeMarker
	case *Event_Decoded:
		return TypeDecoded
	case *Event_Trigger:
		return TypeTrigger
	case *Event_Debug:
		return TypeDebug
	case *Event_Generic:
		return "Generic"
	}
	return "Unknown"
}

// ContentTypeTimelineEvent marks a GenericPayload carrying one JSON-encoded
// timeline event. The core ingests it verbatim (timestamp offset applied), so
// the replay backend can re-emit recorded events losslessly.
const ContentTypeTimelineEvent = "application/vnd.hwlab.timeline-event+json"
