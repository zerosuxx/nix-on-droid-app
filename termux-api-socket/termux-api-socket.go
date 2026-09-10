// termux-api-socket is a general Termux:API bridge for Nix-on-Droid.
//
// Usage:
//
//	termux-api-socket <ApiMethod> [am-style extras...]
//
// It talks directly to the SocketListener of the Nix-on-Droid app, which
// hosts the Termux:API implementation in-process. No `am` invocation and no
// broadcast are involved: the command line is handed over on the app's listen
// socket, which builds the intent and dispatches it to TermuxApiReceiver
// inside the app process. stdin is forwarded to the API and its output is
// written to stdout, so the tool is a drop-in replacement for the upstream
// `termux-api` helper binary (termux-api.c) that the termux-api shell scripts
// invoke.
//
// Extras use the same syntax as `am broadcast` and as upstream's helper:
//
//	--es|-e|--esa NAME VALUE   string / string array (value gets quoted for us)
//	--ez NAME true|false       boolean
//	--ei NAME 42               int
//	--ef NAME 4.2              float
//	--eia NAME 1,2,3           int array
//	--ela NAME 1,2,3           long array
//	-a ACTION                  intent action
//
// For example, setting the clipboard is:
//
//	termux-api-socket Clipboard -e api_version 2 --ez set true
//
// The socket to contact is configurable through the environment, so one binary
// can target a differently named app without a rebuild:
//
//	TERMUX_API_PACKAGE_NAME     package name, "://listen" is appended
//	TERMUX_API_LISTEN_ADDRESS   full address, overrides the above
//
// Wire protocol (see SocketListener.java and termux-api.c):
//
//  1. connect to the abstract socket "<TERMUX_API_PACKAGE_NAME>://listen"
//  2. send a big-endian uint16 length, then that many bytes of UTF-8
//     command line in `am` extra syntax
//  3. read the reply: a single 0x00 byte means the command was parsed and
//     dispatched, anything else is an error message
//
// The API implementation then connects back to the two abstract sockets named
// in the socket_input/socket_output extras to exchange the payload.
package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultAPIPackageName is the build time default for the API package name.
// On Nix-on-Droid TERMUX_PACKAGE_NAME is "com.termux.nix", so the API package
// name — used only as a socket label here, there is no such app installed — is
// "com.termux.nix.api".
const defaultAPIPackageName = "com.termux.nix.api"

// Environment variables overriding which socket we talk to, so the same binary
// works against a differently named app (a debug flavour, upstream's
// "com.termux.api", a fork) without a rebuild.
const (
	// apiPackageNameEnv replaces the package name, keeping the "://listen"
	// suffix that SocketListener appends.
	apiPackageNameEnv = "TERMUX_API_PACKAGE_NAME"
	// listenAddressEnv replaces the whole address, for the case where the
	// listen socket does not follow the "<package>://listen" convention.
	listenAddressEnv = "TERMUX_API_LISTEN_ADDRESS"
)

// listenAddress returns the abstract socket to contact. It must match
// SocketListener.LISTEN_ADDRESS, which is
// TermuxConstants.TERMUX_API_PACKAGE_NAME + "://listen".
func listenAddress() string {
	if address := os.Getenv(listenAddressEnv); address != "" {
		return address
	}

	packageName := os.Getenv(apiPackageNameEnv)
	if packageName == "" {
		packageName = defaultAPIPackageName
	}

	return packageName + "://listen"
}

// connectTimeout bounds how long we wait for the app to connect back to our
// sockets, so a missing or wedged API implementation fails instead of hanging.
const connectTimeout = 15 * time.Second

// usage is printed when no API method is given, or on -h/--help. The examples
// are taken from the upstream termux-api shell scripts, so the extra names and
// types match what the API implementations actually read.
const usage = `usage: %[1]s <ApiMethod> [am-style extras...]

A general Termux:API bridge: hands the command line to the Nix-on-Droid app's
SocketListener over an abstract unix socket, forwards stdin to the API and
writes the API's output to stdout. Drop-in replacement for the upstream
libexec/termux-api helper binary.

Extra syntax (same as ` + "`am broadcast`" + `):

  --es|-e NAME VALUE     string          --ei  NAME 42       int
  --esa NAME A,B,C       string array    --ef  NAME 4.2      float
  --ez NAME true|false   boolean         --eia NAME 1,2,3    int array
  -a ACTION              intent action   --ela NAME 1,2,3    long array

Only string extras (--es/-e/--esa) get their value quoted; everything else is
passed through verbatim, because the app's parser matches those unquoted.

Environment:

  TERMUX_API_PACKAGE_NAME     app package name, "://listen" is appended
                              (default %[2]q)
  TERMUX_API_LISTEN_ADDRESS   full abstract socket address, overrides the above

  TERMUX_API_PACKAGE_NAME=com.termux.api %[1]s BatteryStatus

Clipboard:
  echo hi | %[1]s Clipboard -e api_version 2 --ez set true   # copy
  %[1]s Clipboard                                            # paste

Notifications:
  echo body | %[1]s Notification --es title "Hello" --es id my-note
  echo body | %[1]s Notification --es title "Build" --es priority high \
      --es button_text_1 Retry --es button_action_1 "make retry"
  %[1]s NotificationRemove --es id my-note
  %[1]s NotificationList
  %[1]s NotificationChannel --es id my-chan --es name "My channel"
  %[1]s NotificationChannel --es id my-chan --ez delete true
  # NotificationReply exists too, but it is only dispatched by the reply
  # button of a notification, since it needs a RemoteInput bundle.

Toast / dialogs / speech:
  echo "hello" | %[1]s Toast --ez short true --es gravity middle
  echo "hello" | %[1]s Toast --es text_color red --es background white
  %[1]s Dialog --es input_title "Name?" --es input_hint "type here"
  %[1]s Dialog --es input_method confirm --es input_title "Sure?"
  %[1]s Dialog --es input_method text --ez password true
  echo "spoken text" | %[1]s TextToSpeech --es language en --ef pitch 1.0 --ef rate 1.0
  %[1]s SpeechToText

Device state (no extras needed):
  %[1]s BatteryStatus
  %[1]s WifiConnectionInfo
  %[1]s WifiScanInfo
  %[1]s TelephonyDeviceInfo
  %[1]s TelephonyCellInfo
  %[1]s AudioInfo
  %[1]s CameraInfo
  %[1]s InfraredFrequencies
  %[1]s NotificationList
  %[1]s ContactList
  %[1]s CallLog

Hardware:
  %[1]s Vibrate --ei duration_ms 500 --ez force true
  %[1]s Torch --ez enabled true
  %[1]s Brightness --ei brightness 128 --ez auto false
  %[1]s Brightness --ez auto true
  %[1]s Volume -a set-volume --es stream music --ei volume 7
  %[1]s Sensor --ez all true --ei delay 1000 --ei limit 10
  %[1]s Sensor --es sensors accelerometer --ei limit 5
  %[1]s InfraredTransmit --ei frequency 38000 --eia pattern 20,50,20,50
  %[1]s CameraPhoto --es camera 0 --es file /sdcard/photo.jpg
  %[1]s MicRecorder -a record --es file /sdcard/rec.m4a
  %[1]s Fingerprint
  %[1]s Nfc -a read
  %[1]s WifiEnable --ez enabled true

Location:
  %[1]s Location --es provider gps --es request once
  %[1]s Location --es provider network --es request last

Files, sharing, media:
  %[1]s Download --es url https://example.com/f.zip --es title "f.zip"
  %[1]s Share --es file /sdcard/a.txt --es action send --es content-type text/plain
  echo text | %[1]s Share --es title "Note" --ez default-receiver true
  %[1]s MediaPlayer -a play --es file /sdcard/song.mp3
  %[1]s MediaPlayer -a info
  %[1]s MediaScanner --esa files /sdcard/song.mp3
  %[1]s StorageGet --es file /sdcard/out.bin
  %[1]s Wallpaper --es file /sdcard/bg.jpg --ez lockscreen false

SMS / telephony:
  %[1]s SmsInbox --ei limit 10 --ez conversation-list false
  echo "message" | %[1]s SmsSend --esa recipients "+3612345678" --ei slot 0
  %[1]s TelephonyCall --es number "+3612345678"

Other:
  %[1]s Keystore -a list
  %[1]s JobScheduler --es script /path/to.sh --ei period_ms 900000 --ez persisted true
  %[1]s JobScheduler --ez pending true
  %[1]s SAF -a list
  %[1]s Usb -a list
`

// stringExtraFlags are the option names whose value SocketListener's
// EXTRA_STRING pattern expects in double quotes. Every other option is passed
// through verbatim, matching how termux-api.c builds its command line.
var stringExtraFlags = map[string]bool{
	"-e":    true,
	"--es":  true,
	"--esa": true,
}

// programName is what we prefix diagnostics with, so a symlinked binary
// reports the name it was invoked as.
func programName() string {
	return filepath.Base(os.Args[0])
}

func randomSocketName() (string, error) {
	b := make([]byte, 16)

	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return "termux-api-" + hex.EncodeToString(b), nil
}

// quoteExtra renders a string extra value the way SocketListener's EXTRA_STRING
// pattern expects it: wrapped in double quotes, with inner quotes backslash
// escaped. The pattern is `(-e|--es|--esa) +([^ ]+) +"(.*?)(?<!\\)"`, so an
// unquoted value would not match at all and the parser would reject the
// command line as "Unsupported options".
func quoteExtra(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

// buildCommandLine assembles the `am`-syntax command line for a call to
// apiMethod with the user supplied extras. Input and output are reversed
// relative to our own naming: what we read from is the app's output and vice
// versa.
func buildCommandLine(inputAddress, outputAddress, apiMethod string, extras []string) string {
	parts := []string{
		"--es socket_input " + quoteExtra(outputAddress),
		"--es socket_output " + quoteExtra(inputAddress),
		"--es api_method " + quoteExtra(apiMethod),
	}

	for i := 0; i < len(extras); i++ {
		if !stringExtraFlags[extras[i]] {
			// Booleans, ints, floats, arrays and -a are matched by
			// patterns that take the value unquoted, so these are
			// forwarded as typed.
			parts = append(parts, extras[i])
			continue
		}

		// A string extra is a three token group: flag, name, value. Only
		// the value is quoted; a truncated group is forwarded as is and
		// left for the app to reject.
		group := extras[i:min(i+3, len(extras))]
		parts = append(parts, group...)

		if len(group) == 3 {
			parts[len(parts)-1] = quoteExtra(group[2])
		}

		i += len(group) - 1
	}

	return strings.Join(parts, " ")
}

// sendToListener hands the command line to the app and waits for the
// acknowledgement byte.
func sendToListener(cmdline string) error {
	address := listenAddress()

	conn, err := net.Dial("unix", "@"+address)
	if err != nil {
		return fmt.Errorf("connect to %q: %w", address, err)
	}
	defer conn.Close()

	if len(cmdline) > math.MaxUint16 {
		return fmt.Errorf("command line of %d bytes exceeds the 16-bit length prefix", len(cmdline))
	}

	buf := make([]byte, 2+len(cmdline))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(cmdline)))
	copy(buf[2:], cmdline)

	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("send command line: %w", err)
	}

	reply := make([]byte, 256)
	n, err := conn.Read(reply)
	if err != nil {
		return fmt.Errorf("read acknowledgement: %w", err)
	}

	if n == 1 && reply[0] == 0 {
		return nil
	}

	return fmt.Errorf("plugin reported an error: %s", strings.TrimSpace(string(reply[:n])))
}

func run(apiMethod string, extras []string) error {
	// inputAddress:  API -> us -> stdout
	// outputAddress: stdin -> us -> API
	inputAddress, err := randomSocketName()
	if err != nil {
		return fmt.Errorf("generate input socket name: %w", err)
	}

	outputAddress, err := randomSocketName()
	if err != nil {
		return fmt.Errorf("generate output socket name: %w", err)
	}

	// Go maps a leading @ to the Linux abstract socket namespace, which is
	// what the Java side's LocalSocketAddress.Namespace.ABSTRACT expects.
	inputListener, err := net.Listen("unix", "@"+inputAddress)
	if err != nil {
		return fmt.Errorf("listen on input socket: %w", err)
	}
	defer inputListener.Close()

	outputListener, err := net.Listen("unix", "@"+outputAddress)
	if err != nil {
		return fmt.Errorf("listen on output socket: %w", err)
	}
	defer outputListener.Close()

	// The sockets have to exist before the app is told about them.
	if err := sendToListener(buildCommandLine(inputAddress, outputAddress, apiMethod, extras)); err != nil {
		return err
	}

	// Only the input socket is guaranteed to be used: ResultReturner always
	// connects back to it to write the result, but it connects to the output
	// socket only when the API's result writer is a WithInput subclass, i.e.
	// when that API actually consumes stdin. Most APIs (BatteryStatus, a plain
	// Clipboard paste, ...) never do, so waiting for the output side to be
	// accepted would hang forever. Upstream's termux-api.c has the same shape:
	// it joins only the socket-to-stdout transfer and lets the process exit
	// with the stdin thread still parked in accept().
	//
	// So the input socket drives the run, and the stdin feeder is best effort.

	// Closing the listener makes a pending Accept fail, which is how the
	// timeout reaches the goroutine below. Cancelled once the API has
	// connected, so that a slow transfer is not interrupted.
	timedOut := make(chan struct{})
	timeout := time.AfterFunc(connectTimeout, func() {
		close(timedOut)
		inputListener.Close()
		outputListener.Close()
	})
	defer timeout.Stop()

	// stdin -> output socket -> API. Never waited on: if this API does not
	// read stdin nobody will ever connect, and the goroutine stays parked in
	// Accept until the process exits. Errors are logged rather than returned
	// for the same reason — there is no one left to report them to once the
	// result has been written.
	go func() {
		conn, err := outputListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		if _, err := io.Copy(conn, os.Stdin); err != nil {
			fmt.Fprintf(os.Stderr, "%s: copy stdin to API: %v\n", programName(), err)
			return
		}

		// The API reads until EOF, so half-close instead of waiting for the
		// deferred full Close.
		if unixConn, ok := conn.(*net.UnixConn); ok {
			if err := unixConn.CloseWrite(); err != nil {
				fmt.Fprintf(os.Stderr, "%s: half-close output socket: %v\n", programName(), err)
			}
		}
	}()

	// API -> input socket -> stdout
	conn, err := inputListener.Accept()
	if err != nil {
		select {
		case <-timedOut:
			return fmt.Errorf("the API implementation did not connect back within %s", connectTimeout)
		default:
		}

		return fmt.Errorf("accept on input socket: %w", err)
	}
	defer conn.Close()

	// The transfer itself is not bounded by connectTimeout.
	timeout.Stop()

	if _, err := io.Copy(os.Stdout, conn); err != nil {
		return fmt.Errorf("copy API output to stdout: %w", err)
	}

	return nil
}

func main() {
	name := programName()

	// No arguments is a usage error, an explicit --help is not, so the help
	// goes to stderr/stdout accordingly and the exit status differs.
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, name, defaultAPIPackageName)
		os.Exit(2)
	}

	if os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Printf(usage, name, defaultAPIPackageName)
		return
	}

	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		os.Exit(1)
	}
}
