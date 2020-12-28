package components

import (
	"os"
	"fmt"
	"time"
	"io/ioutil"
	"encoding/json"
	"github.com/kerberos-io/opensource/backend/src/models"
)

func ReadUserConfig() models.User {
	var userConfig models.User

	// Open global config
	for {
		jsonFile, err := os.Open("./data/config/user.json")
		if err != nil {
			fmt.Println(err)
			fmt.Println("Config file is not found " + "./data/config/user.json" + ", trying again in 5s.")
			time.Sleep(5 * time.Second)
		} else {
			fmt.Println("Successfully Opened user.json")
			byteValue, _ := ioutil.ReadAll(jsonFile)
			err = json.Unmarshal(byteValue, &userConfig)
			if err != nil {
				fmt.Println("JSON file not valid: " + err.Error())
			} else {
				jsonFile.Close()
				break
			}
			time.Sleep(5 * time.Second)
		}
		jsonFile.Close()
	}

	return userConfig
}