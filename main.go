package main

import (
	"flag"
	"hivpn/config"
	"hivpn/log"
	"hivpn/utils"
	"hivpn/vpn"
	"os"
	"runtime"
)

var (
	configPath string
	logLevel   int
	ServerMode bool
)

func init() {
	flag.StringVar(&configPath, "config", "config.toml", "location of the config file")
	flag.BoolVar(&ServerMode, "S", false, "server mode")
	flag.IntVar(&logLevel, "l", log.LevelInfo, "log level: [0-DEBUG 1-INFO 2-ERROR]")
	runtime.GOMAXPROCS(runtime.NumCPU())
}

func main() {
	flag.Parse()
	log.SetLevel(logLevel)
	log.Debug("Load Config from", configPath)
	conf, err := config.Load(configPath)
	if err != nil {
		log.Error("start error:", err)
		os.Exit(1)
	}

	var usersAuthen []vpn.User
	if ServerMode {
		for _, u := range conf.Users {
			usersAuthen = append(usersAuthen, vpn.User{
				IP:   u.Ipaddress,
				Name: u.Username,
				Pass: u.Password,
			})
		}
	} else {
		newDomain, newHost, err := utils.ValidServer(conf.Server)
		if err != nil {
			log.Error(err)
			os.Exit(1)
		}
		conf.Server = newHost
		if len(conf.HostHeader) < 1 {
			conf.HostHeader = newDomain
		}
		usersAuthen = append(usersAuthen, vpn.User{
			Name: conf.User,
			Pass: conf.Pass,
			IP:   conf.Address,
		})
	}

	_, err = vpn.Create(vpn.Config{
		MTU:            conf.MTU,
		ServerAddr:     conf.Server,
		LocalAddr:      conf.Address,
		HostHeader:     conf.HostHeader,
		DefaultGateway: conf.DefaultGateway,
		IsServer:       ServerMode,
		Users:          usersAuthen,
		Whitelist:      conf.Whitelist,
		Blacklist:      conf.Blacklist,
	})
	if err != nil {
		log.Error("Cannot start tunnel vpn:", err)
	}

}
