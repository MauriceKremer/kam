package kiss

import (
	"math"
	"time"
)

// loraPreambleSymbols is the preamble length used for airtime estimates. The
// modem's GetAirtime command (RadioClient.AirtimeMillis) is authoritative; this
// estimate only has to be close enough for duty-cycle accounting.
const loraPreambleSymbols = 8

// LoRaAirtime estimates the time on air of a LoRa frame with an explicit
// header and payload CRC (Semtech AN1200.13). codingRate uses the KISS modem's
// 5-8 encoding for 4/5-4/8. It returns 0 for parameters outside the chip's
// range.
func LoRaAirtime(spreadingFactor uint8, bandwidthHz uint32, codingRate uint8, payloadBytes int) time.Duration {
	if spreadingFactor < 5 || spreadingFactor > 12 || bandwidthHz == 0 || codingRate < 5 || codingRate > 8 || payloadBytes < 0 {
		return 0
	}
	sf := float64(spreadingFactor)
	symbolSeconds := math.Pow(2, sf) / float64(bandwidthHz)
	lowDataRate := 0.0
	if symbolSeconds > 0.016 {
		lowDataRate = 1
	}
	preambleSymbols := float64(loraPreambleSymbols) + 4.25
	if spreadingFactor < 7 {
		preambleSymbols = float64(loraPreambleSymbols) + 6.25
	}
	const crcBytes, implicitHeader = 1.0, 0.0
	numerator := 8*float64(payloadBytes) - 4*sf + 28 + 16*crcBytes - 20*implicitHeader
	denominator := 4 * (sf - 2*lowDataRate)
	payloadSymbols := 8 + math.Max(math.Ceil(numerator/denominator)*float64(codingRate), 0)
	seconds := (preambleSymbols + payloadSymbols) * symbolSeconds
	return time.Duration(seconds * float64(time.Second))
}
