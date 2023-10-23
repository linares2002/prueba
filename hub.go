// Copyright 2022 Arduino SA
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"os"
	"os/user"
   "path/filepath"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"io/ioutil"
	"github.com/micmonay/keybd_event"

	"net/http"
	"github.com/arduino/arduino-create-agent/utilities"

	cert "github.com/arduino/arduino-create-agent/certificates"
	"github.com/arduino/arduino-create-agent/config"

	"github.com/arduino/arduino-create-agent/upload"
	"log"
	"encoding/base64"
	"bufio"	
)

type hub struct {
	// Registered connections.
	connections map[*connection]bool

	// Inbound messages from the connections.
	broadcast chan []byte

	// Inbound messages from the system
	broadcastSys chan []byte

	// Register requests from the connections.
	register chan *connection

	// Unregister requests from connections.
	unregister chan *connection
}

var h = hub{
	broadcast:    make(chan []byte, 1000),
	broadcastSys: make(chan []byte, 1000),
	register:     make(chan *connection),
	unregister:   make(chan *connection),
	connections:  make(map[*connection]bool),
}

const commands = `{
  "Commands": [
    "list",
    "open <portName> <baud> [bufferAlgorithm: ({default}, timed, timedraw)]",
    "(send, sendnobuf, sendraw) <portName> <cmd>",
    "close <portName>",
    "restart",
    "exit",
    "killupload",
    "downloadtool <tool> <toolVersion: {latest}> <pack: {arduino}> <behaviour: {keep}>",
    "upload <portName> <FQBN> <base64(code)>",
    "lib {install, uninstall} <library>">,
    "log",
    "memorystats",
    "gc",
    "hostname",
    "version"
  ]
}`

func (h *hub) unregisterConnection(c *connection) {
	if _, contains := h.connections[c]; !contains {
		return
	}
	delete(h.connections, c)
	close(c.send)
}

func (h *hub) sendToRegisteredConnections(data []byte) {
	for c := range h.connections {
		select {
		case c.send <- data:
			//log.Print("did broadcast to ")
			//log.Print(c.ws.RemoteAddr())
			//c.send <- []byte("hello world")
		default:
			h.unregisterConnection(c)
		}
	}
}

func (h *hub) run() {
	for {
		select {
		case c := <-h.register:
			h.connections[c] = true
			// send supported commands
			c.send <- []byte(fmt.Sprintf(`{"Version" : "%s"} `, version))
			c.send <- []byte(html.EscapeString(commands))
			c.send <- []byte(fmt.Sprintf(`{"Hostname" : "%s"} `, *hostname))
			c.send <- []byte(fmt.Sprintf(`{"OS" : "%s"} `, runtime.GOOS))
		case c := <-h.unregister:
			h.unregisterConnection(c)
		case m := <-h.broadcast:
			if len(m) > 0 {
				checkCmd(m)
				h.sendToRegisteredConnections(m)
			}
		case m := <-h.broadcastSys:
			h.sendToRegisteredConnections(m)
		}
	}
}

func checkCmd(m []byte) {
	// log.Print("Inside checkCmd")
	s := string(m[:])
	// log.Print(s)

	sl := strings.ToLower(strings.Trim(s, "\n"))

	if *hibernate {
		//do nothing
		return
	}

	if strings.HasPrefix(sl, "open") {

		args := strings.Split(s, " ")
		if len(args) < 3 {
			go spErr("You did not specify a port and baud rate in your open cmd")
			return
		}
		if len(args[1]) < 1 {
			go spErr("You did not specify a serial port")
			return
		}

		baudStr := strings.Replace(args[2], "\n", "", -1)
		baud, err := strconv.Atoi(baudStr)
		if err != nil {
			go spErr("Problem converting baud rate " + args[2])
			return
		}
		// pass in buffer type now as string. if user does not
		// ask for a buffer type pass in empty string
		bufferAlgorithm := "default" // use the default buffer if none is specified
		if len(args) > 3 {
			// cool. we got a buffer type request
			buftype := strings.Replace(args[3], "\n", "", -1)
			bufferAlgorithm = buftype
		}
		go spHandlerOpen(args[1], baud, bufferAlgorithm)

	} else if strings.HasPrefix(sl, "close") {

		args := strings.Split(s, " ")
		if len(args) > 1 {
			go spClose(args[1])
		} else {
			go spErr("You did not specify a port to close")
		}

	} else if strings.HasPrefix(sl, "killupload") {
		// kill the running process (assumes singleton for now)
		go func() {
			upload.Kill()
			h.broadcastSys <- []byte("{\"uploadStatus\": \"Killed\"}")
			log.Println("{\"uploadStatus\": \"Killed\"}")
		}()

	} else if strings.HasPrefix(sl, "send") {
		// will catch send and sendnobuf and sendraw
		go spWrite(s)
	} else if strings.HasPrefix(sl, "list") {
		go spList(false)
		go spList(true)
	} else if strings.HasPrefix(sl, "downloadtool") {
		// Always delete root certificates when we receive a downloadtool command
		// Useful if the install procedure was not followed strictly (eg. manually)
		cert.DeleteCertificates(config.GetCertificatesDir())
		go func() {
			args := strings.Split(s, " ")
			var tool, toolVersion, pack, behaviour string
			toolVersion = "latest"
			pack = "arduino"
			behaviour = "keep"
			if len(args) <= 1 {
				mapD := map[string]string{"DownloadStatus": "Error", "Msg": "Not enough arguments"}
				mapB, _ := json.Marshal(mapD)
				h.broadcastSys <- mapB
				return
			}
			if len(args) > 1 {
				tool = args[1]
			}
			if len(args) > 2 {
				if strings.HasPrefix(args[2], "http") {
					//old APIs, ignore this field
				} else {
					toolVersion = args[2]
				}
			}
			if len(args) > 3 {
				pack = args[3]
			}
			if len(args) > 4 {
				behaviour = args[4]
			}
			
            // download tool arduino-cli extension para TFG
            if tool == "arduino-cli" && len(args) == 2{
			      usr, _ := user.Current()
				  mapD := map[string]string{"DownloadStatus": "Pending", "Msg": "Downloading"}
				  mapB, _ := json.Marshal(mapD)
				  h.broadcastSys <- mapB
				  path := usr.HomeDir + "/.arduino-create/arduino/"
				  file_check := path + "arduino-cli/arduino-cli.exe"
				  if _, err := os.Stat(file_check); err == nil {
					  mapD = map[string]string{"DownloadStatus": "Success", "Msg": "Tool exists"}
					  mapB, _ = json.Marshal(mapD)
					  h.broadcastSys <- mapB
				  } else {
					  filepath := path + "arduino-cli.zip"
					  fileUrl := "https://downloads.arduino.cc/arduino-cli/nightly/arduino-cli_nightly-latest_Windows_64bit.zip"
					  // Get the data
					  resp, err := http.Get(fileUrl)
					  defer resp.Body.Close()
					  // Create the file
					  out, err := os.Create(filepath)
					  defer out.Close()
					  mapD = map[string]string{"DownloadStatus": "Pending", "Msg": "Unpacking"}
					  mapB, _ = json.Marshal(mapD)
					  h.broadcastSys <- mapB				  
					  // Write the body to file
					  _, err = io.Copy(out, resp.Body)				  
					  err = utilities.Unzip(filepath, path+"arduino-cli")
						  if err != nil {
							panic(err)
					  }					  
					  mapD = map[string]string{"DownloadStatus": "Success", "Msg": "Tool installed"}
					  mapB, _ = json.Marshal(mapD)
					  h.broadcastSys <- mapB
				  }
            } else{		
			    err := Tools.Download(pack, tool, toolVersion, behaviour)
			    if err != nil {
				  mapD := map[string]string{"DownloadStatus": "Error", "Msg": err.Error()}
				  mapB, _ := json.Marshal(mapD)
			      h.broadcastSys <- mapB
			   } else {
				  mapD := map[string]string{"DownloadStatus": "Success", "Msg": "Map Updated"}
				  mapB, _ := json.Marshal(mapD)
				  h.broadcastSys <- mapB
			   }
			}
			

		}()
	} else if strings.HasPrefix(sl, "log") {
		go logAction(sl)
	} else if strings.HasPrefix(sl, "restart") {
		log.Println("Received restart from the daemon. Why? Boh")
		Systray.Restart()
	} else if strings.HasPrefix(sl, "exit") {
		Systray.Quit()
	} else if strings.HasPrefix(sl, "memstats") {
		memoryStats()
	} else if strings.HasPrefix(sl, "gc") {
		garbageCollection()
	} else if strings.HasPrefix(sl, "hostname") {
		getHostname()
	} else if strings.HasPrefix(sl, "version") {
		getVersion()
	} else if strings.HasPrefix(sl, "wokwi") {
		getWokwi()	
	} else if strings.HasPrefix(sl, "upload") {
	    args := strings.Split(s, " ")
		getUpload(args[1], args[2], args[3])
	} else if strings.HasPrefix(sl, "lib") {
	    args := strings.Split(s, " ")
		if len(args) <= 2 {
			mapD := map[string]string{"DownloadStatus": "Error", "Msg": "Not enough arguments"}
			mapB, _ := json.Marshal(mapD)
			h.broadcastSys <- mapB
		} else {
			getLib(args[1], args[2])
		}
	} else {
		go spErr("Could not understand command.")
	}
}

func logAction(sl string) {
	if strings.HasPrefix(sl, "log on") {
		*logDump = "on"
		multiWriter := io.MultiWriter(&loggerWs, os.Stderr)
		log.SetOutput(multiWriter)
	} else if strings.HasPrefix(sl, "log off") {
		*logDump = "off"
		log.SetOutput(os.Stderr)
		// } else if strings.HasPrefix(sl, "log show") {
		// TODO: send all the saved log to websocket
		//h.broadcastSys <- []byte("{\"BufFlowDebug\" : \"" + *logDump + "\"}")
	}
}

func memoryStats() {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	json, _ := json.Marshal(memStats)
	log.Printf("memStats:%v\n", string(json))
	h.broadcastSys <- json
}

func getHostname() {
	h.broadcastSys <- []byte("{\"Hostname\" : \"" + *hostname + "\"}")
}

func getVersion() {
	h.broadcastSys <- []byte("{\"Version\" : \"" + version + "\"}")
}

func getUpload(s1 string, s2 string, s3 string) {
    rawDecodedText, err := base64.StdEncoding.DecodeString(s3)
    if err != nil {
        panic(err)
    }
	
	usr, _ := user.Current()
	path := usr.HomeDir + "/.arduino-create/arduino/arduino-cli"
	dir, err := ioutil.TempDir(path, "")
	if err != nil {
		log.Fatal(err)
	}
	project_name := filepath.Base(dir)
	filepath := dir + "/" + project_name + ".ino"
	file, err := os.Create(filepath)
    if err != nil {
        log.Fatal(err)
    }
    defer file.Close()
    bytes := []byte(rawDecodedText)
    file.Write(bytes)
	
	tool := path + "/arduino-cli"
	command1 := "compile"
	command2 := "upload"
	port := "-p" + s1
	fqdn := "-b" + s2
	sketch := dir
	fmt.Println(s2)
	
	cmd := exec.Command(tool, command1, port, fqdn, sketch);
	//cmd.Stdout = os.Stdout
	//cmd.Stderr = os.Stderr
    cmdReader, err := cmd.StdoutPipe()
    if err != nil {
        fmt.Fprintln(os.Stderr, "Error creating StdoutPipe for Cmd", err)
        return
    }
    scanner := bufio.NewScanner(cmdReader)
    go func() {
        for scanner.Scan() {
			h.broadcastSys <- []byte(scanner.Text())
        }
    }()

	err = cmd.Run()
	if err != nil {
		log.Println("cmd.Run() failed with %s\n", err)
	}

	
	cmd = exec.Command(tool, command2, port, fqdn, sketch)
	err = cmd.Run()
	if err != nil {
		log.Println("cmd.Run() failed with %s\n", err)
	}

	h.broadcastSys <- []byte("\r\nEl archivo se ha cargado en el dispositivo correctamente\r\n\r\n")
}



func getWokwi() {
    time.Sleep(4 * time.Second)
	kb, err := keybd_event.NewKeyBonding()
	if err != nil {
		panic(err)
	}	

	// Select keys to be pressed
	kb.SetKeys(keybd_event.VK_A, keybd_event.VK_V) 

	// Set shift to be pressed
	kb.HasCTRL(true) 

	// Press the selected keys
	err = kb.Launching() 
	if err != nil {
		panic(err)
	}
	
	// Or you can use Press and Release
	kb.Press()
	time.Sleep(1 * time.Millisecond)
	kb.Release()
	
	h.broadcastSys <- []byte("{\"wokwi\" : \"on\"}")
}


func getLib(s1 string, s2 string) {
	mapD := map[string]string{"LibStatus": "Error", "Msg": "Unknown command"}
	usr, _ := user.Current()
	path := usr.HomeDir + "/.arduino-create/arduino/arduino-cli"
	tool := path + "/arduino-cli"

	s2 = strings.ReplaceAll(s2, "+", " ")
	fmt.Println(s2)
	
	
	
	if s1 == "install" {
		mapD = map[string]string{"LibStatus": "Success", "Msg": "Library installed"}
	}
	if s1 == "uninstall" {
		mapD = map[string]string{"LibStatus": "Success", "Msg": "Library uninstalled"}
	}
	
	cmd := exec.Command(tool, "lib", s1, s2);
	fmt.Println(cmd)
    cmdReader, err := cmd.StdoutPipe()
    if err != nil {
        fmt.Fprintln(os.Stderr, "Error creating StdoutPipe for Cmd", err)
        return
    }
    scanner := bufio.NewScanner(cmdReader)
    go func() {
        for scanner.Scan() {
			h.broadcastSys <- []byte(scanner.Text())
        }
    }()

	err = cmd.Run()
	if err != nil {
		log.Println("cmd.Run() failed with %s\n", err)
	}

	mapB, _ := json.Marshal(mapD)
	h.broadcastSys <- mapB
}



func garbageCollection() {
	log.Printf("Starting garbageCollection()\n")
	h.broadcastSys <- []byte("{\"gc\":\"starting\"}")
	memoryStats()
	debug.SetGCPercent(100)
	debug.FreeOSMemory()
	debug.SetGCPercent(-1)
	log.Printf("Done with garbageCollection()\n")
	h.broadcastSys <- []byte("{\"gc\":\"done\"}")
	memoryStats()
}
