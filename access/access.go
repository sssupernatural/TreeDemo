package main

import (
	"flag"
	"github.com/BurntSushi/toml"
	"fmt"
	"access/server"
	"os"
	"access/rpc"
)

type AccessConfig struct {
	Addr        string
	LogPath     string
	UMAddr      string
	TMAddr      string
}

var (
	accessConfigPath string
	accessConf       AccessConfig
)

func init() {
	flag.StringVar(&accessConfigPath, "conf", "./accessConf.toml", "Access Config Path")
}

func initAccessLog(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0777)
	if err != nil {
		return err
	}

	server.SetServerLogger(file)
	rpc.SetRPCLogger(file)

	return nil
}

func main() {
	flag.Parse()

	_, err := toml.DecodeFile(accessConfigPath, &accessConf)
	if err != nil {
		fmt.Printf("Access decode toml config file failed! error : %s.\n", err)
		return
	}

	err = initAccessLog(accessConf.LogPath)
	if err != nil {
		fmt.Println("Initialize access log failed.")
		return
	}

	fmt.Printf("Access Addr:[%s], Log Path:[%s], UM Addr:[%s], TM Addr:[%s].\n",
		accessConf.Addr, accessConf.LogPath, accessConf.UMAddr, accessConf.TMAddr)

	accessServer := server.New()

	accessServer.Addr = accessConf.Addr
	accessServer.UMAddr = accessConf.UMAddr
	accessServer.TMAddr = accessConf.TMAddr

	err = accessServer.Serve()
	if err != nil {
		fmt.Printf("Start Access Server Failed ! ERROR : %s\n", err)

		return
	}

}