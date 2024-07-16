package pn532_uart

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/wizzomafizzo/tapto/pkg/config"
	"github.com/wizzomafizzo/tapto/pkg/readers"
	"github.com/wizzomafizzo/tapto/pkg/tokens"
	"github.com/wizzomafizzo/tapto/pkg/utils"

	"go.bug.st/serial"
)

type Pn532UartReader struct {
	cfg       *config.UserConfig
	device    string
	name      string
	polling   bool
	port      serial.Port
	lastToken *tokens.Token
}

func NewReader(cfg *config.UserConfig) *Pn532UartReader {
	return &Pn532UartReader{
		cfg: cfg,
	}
}

func (r *Pn532UartReader) Ids() []string {
	return []string{"pn532_uart"}
}

func connect(name string) (serial.Port, error) {
	log.Debug().Msgf("connecting to %s", name)
	port, err := serial.Open(name, &serial.Mode{
		BaudRate: 115200,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return port, err
	}

	err = port.SetReadTimeout(100 * time.Millisecond)
	if err != nil {
		return port, err
	}

	err = SamConfiguration(port)
	if err != nil {
		return port, err
	}

	fv, err := GetFirmwareVersion(port)
	if err != nil {
		return port, err
	}
	log.Debug().Msgf("firmware version: %v", fv)

	gs, err := GetGeneralStatus(port)
	if err != nil {
		return port, err
	}
	log.Debug().Msgf("general status: %v", gs)

	return port, nil
}

func (r *Pn532UartReader) Open(device string, iq chan<- readers.Scan) error {
	ps := strings.SplitN(device, ":", 2)
	if len(ps) != 2 {
		return errors.New("invalid device string: " + device)
	}

	if !utils.Contains(r.Ids(), ps[0]) {
		return errors.New("invalid reader id: " + ps[0])
	}

	name := ps[1]

	if runtime.GOOS != "windows" {
		if _, err := os.Stat(name); err != nil {
			return err
		}
	}

	port, err := connect(name)
	if err != nil {
		if port != nil {
			_ = port.Close()
		}
		return err
	}

	r.port = port
	r.device = device
	r.name = name
	r.polling = true

	go func() {
		errors := 0
		maxErrors := 5

		for r.polling {
			if errors >= maxErrors {
				log.Error().Msg("too many errors, exiting")
				err := r.Close()
				if err != nil {
					log.Warn().Err(err).Msg("failed to close serial port")
				}
				r.polling = false
				break
			}

			time.Sleep(250 * time.Millisecond)

			tgt, err := InListPassiveTarget(r.port)
			if err != nil {
				log.Error().Err(err).Msg("failed to read passive target")
				errors++
				continue
			} else if tgt == nil {
				// token was removed
				if r.lastToken != nil {
					iq <- readers.Scan{
						Source: r.device,
						Token:  nil,
					}
					r.lastToken = nil
				}
				continue
			}

			errors = 0

			if r.lastToken != nil && r.lastToken.UID == tgt.Uid {
				// same token
				continue
			}

			var td TagData
			if tgt.Type == tokens.TypeNTAG {
				td, err = ReadNtag(r.port)
				if err != nil {
					log.Error().Err(err).Msg("failed to read ntag")
					continue
				}
			} else if tgt.Type == tokens.TypeMifare {
				log.Error().Err(err).Msg("mifare not supported")
				continue
			}

			log.Debug().Msgf("record bytes: %s", hex.EncodeToString(td.Bytes))

			tagText, err := ParseRecordText(td.Bytes)
			if err != nil {
				log.Error().Err(err).Msgf("error parsing NDEF record")
				tagText = ""
			}

			if tagText == "" {
				log.Warn().Msg("no text NDEF found")
			} else {
				log.Info().Msgf("decoded text NDEF: %s", tagText)
			}

			token := &tokens.Token{
				Type:     tgt.Type,
				UID:      tgt.Uid,
				Text:     tagText,
				Data:     hex.EncodeToString(td.Bytes),
				ScanTime: time.Now(),
				Source:   r.device,
			}

			if !utils.TokensEqual(token, r.lastToken) {
				iq <- readers.Scan{
					Source: r.device,
					Token:  token,
				}
			}

			r.lastToken = token
		}
	}()

	return nil
}

func (r *Pn532UartReader) Close() error {
	r.polling = false
	if r.port != nil {
		err := r.port.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// keep track of serial devices that had failed opens
var serialCacheMu = &sync.RWMutex{}
var serialBlockList = []string{}

func (r *Pn532UartReader) Detect(connected []string) string {
	ports, err := serial.GetPortsList()
	if err != nil {
		log.Error().Err(err).Msg("failed to get serial ports")
	}
	// log.Debug().Msgf("serial ports: %v", ports)

	toCheck := make([]string, 0)
	for _, name := range ports {
		if runtime.GOOS == "windows" {
			if strings.HasPrefix(name, "COM") {
				toCheck = append(toCheck, name)
			}
		} else if runtime.GOOS == "darwin" {
			if strings.HasPrefix(name, "/dev/tty.") {
				toCheck = append(toCheck, name)
			}
		} else {
			if strings.HasPrefix(name, "/dev/ttyUSB") || strings.HasPrefix(name, "/dev/ttyACM") {
				toCheck = append(toCheck, name)
			}
		}
	}

	for _, name := range toCheck {
		device := "pn532_uart:" + name

		// ignore if device is in block list
		serialCacheMu.RLock()
		if utils.Contains(serialBlockList, name) {
			serialCacheMu.RUnlock()
			continue
		}
		serialCacheMu.RUnlock()

		// ignore if exact same device and reader are connected
		if utils.Contains(connected, device) {
			continue
		}

		if runtime.GOOS != "windows" {
			// resolve device symlink if necessary
			realPath := ""
			symPath, err := os.Readlink(name)
			if err == nil {
				parent := filepath.Dir(name)
				abs, err := filepath.Abs(filepath.Join(parent, symPath))
				if err == nil {
					realPath = abs
				}
			}

			// ignore if same resolved device and reader connected
			if realPath != "" && utils.Contains(connected, realPath) {
				continue
			}

			// ignore if different resolved device and reader connected
			if realPath != "" && strings.HasSuffix(realPath, ":"+realPath) {
				continue
			}
		}

		// ignore if different reader already connected
		match := false
		for _, connDev := range connected {
			if strings.HasSuffix(connDev, ":"+name) {
				match = true
				break
			}
		}
		if match {
			continue
		}

		// try to open the device
		port, err := connect(name)
		if err != nil {
			log.Debug().Err(err).Msgf("failed to open detected serial port, blocklisting: %s", name)
			_ = port.Close()
			serialCacheMu.Lock()
			serialBlockList = append(serialBlockList, name)
			serialCacheMu.Unlock()
			continue
		} else {
			err = port.Close()
			if err != nil {
				log.Warn().Err(err).Msg("failed to close serial port")
			}

			return device
		}
	}

	return ""
}

func (r *Pn532UartReader) Device() string {
	return r.device
}

func (r *Pn532UartReader) Connected() bool {
	return r.polling && r.port != nil
}

func (r *Pn532UartReader) Info() string {
	return "PN532 UART (" + r.name + ")"
}

func (r *Pn532UartReader) Write(text string) (*tokens.Token, error) {
	return nil, errors.New("writing not supported on this reader")
}
