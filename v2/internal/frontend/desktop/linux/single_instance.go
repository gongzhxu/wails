//go:build linux
// +build linux

package linux

import (
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
	"github.com/wailsapp/wails/v2/pkg/options"
)

type dbusHandler func(string)

func (f dbusHandler) SendMessage(message string) *dbus.Error {
	f(message)
	return nil
}

func SetupSingleInstance(uniqueID string) {
	id := "wails_app_" + strings.ReplaceAll(strings.ReplaceAll(uniqueID, "-", "_"), ".", "_")

	dbusName := "org." + id + ".SingleInstance"
	dbusPath := "/org/" + id + "/SingleInstance"

	conn, err := dbus.ConnectSessionBus()
	// if we will reach any error during establishing connection or sending message we will just continue.
	// It should not be the case that such thing will happen actually, but just in case.
	if err != nil {
		return
	}

	f := dbusHandler(func(message string) {
		var secondInstanceData options.SecondInstanceData

		err := json.Unmarshal([]byte(message), &secondInstanceData)
		if err == nil {
			secondInstanceBuffer <- secondInstanceData
		}
	})

	err = conn.Export(f, dbus.ObjectPath(dbusPath), dbusName)
	if err != nil {
		return
	}

	reply, err := conn.RequestName(dbusName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return
	}

	// if name already taken, try to send args to existing instance, if no success just launch new instance
	if reply == dbus.RequestNameReplyExists {
		data := options.SecondInstanceData{
			Args: os.Args[1:],
		}
		data.WorkingDirectory, err = os.Getwd()
		if err != nil {
			log.Printf("Failed to get working directory: %v", err)
			return
		}

		serialized, err := json.Marshal(data)
		if err != nil {
			log.Printf("Failed to marshal data: %v", err)
			return
		}

		err = conn.Object(dbusName, dbus.ObjectPath(dbusPath)).Call(dbusName+".SendMessage", 0, conv.BytesToString(serialized)).Store()
		if err != nil {
			return
		}
		os.Exit(1)
	}
}
