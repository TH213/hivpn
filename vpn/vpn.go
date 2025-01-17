package vpn

import (
	"encoding/base64"
	"fmt"
	"hivpn/connection"
	"hivpn/crypto"
	"hivpn/log"
	"hivpn/network"
	"hivpn/tun"
	"hivpn/utils"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	MTU            int
	ServerAddr     string
	LocalAddr      string
	HostHeader     string
	DefaultGateway string
	IsServer       bool
	Whitelist      []string
	Blacklist      []string
	Users          []User
}

type User struct {
	Name string
	Pass string
	IP   string
}

type VPN struct {
	conf Config

	dev       tun.Device
	arpTable  *network.ARP
	userTable map[string]User
	blackList map[string]bool
	myNetwork *net.IPNet

	writeDevToTun        func(header network.PacketHeader, data []byte) error
	getCurrentConnClient func(ip string) network.ARPRecord
	inMyNetwork          func(ip net.IP) bool
}

const (
	TUN_NAME = "MyNIC"

	TIME_TO_TRY = 5 * time.Second
	MAX_TRY     = 10
	VERSION     = "1.1.0 - (29/11/2022)"
)

var (
	YOUR_OS = runtime.GOOS
)

func Create(conf Config) (vpn *VPN, err error) {
	vpn = new(VPN)
	vpn.conf = conf
	vpn.blackList = make(map[string]bool, 0)
	_, vpn.myNetwork, err = net.ParseCIDR(vpn.conf.LocalAddr)
	if err != nil {
		return
	}

	connectType := connection.CONNECTION_TYPE_WEBSOCKET

	log.Debug("Create Virtual Network Adapter")
	vpn.dev, err = tun.CreateTUN(TUN_NAME, vpn.conf.MTU)
	if err != nil {
		return
	}
	defer vpn.stop()

	virtualChannel := connection.TUN{
		Addr:              vpn.conf.ServerAddr,
		HostHeader:        vpn.conf.HostHeader,
		FuncWriteTunToDev: vpn.writeTunToDev,
		FuncAuthenConn:    vpn.authenConn,
	}
	log.Debug("Make ARP Table")
	vpn.arpTable = network.NewARP()

	log.Debug("Setup Authentication")
	vpn.setupAuthentication()

	var tokenUser = ""
	if !vpn.conf.IsServer {
		for k, v := range vpn.userTable {
			tokenByte, err := crypto.AESEncrypt([]byte(v.Pass), []byte(utils.GenUUID()))
			if err != nil {
				return nil, err
			}
			tokenUser = k + ":" + base64.StdEncoding.EncodeToString(tokenByte)
			break
		}
		log.Debug("Your token:", tokenUser)
		vpn.getCurrentConnClient = vpn.arpTable.QueryOne
		vpn.inMyNetwork = func(ip net.IP) bool {
			return false
		}
	} else {
		vpn.inMyNetwork = func(ip net.IP) bool {
			return vpn.myNetwork.Contains(ip)
		}
		vpn.getCurrentConnClient = vpn.arpTable.Query
	}

	err = virtualChannel.Connect(tokenUser, connectType)
	if err != nil {
		return
	}

	log.Debug("Route Network")
	err = vpn.setupRoute()
	if err != nil {
		return
	}

	vpn.OnFuncWriteDevToTun(virtualChannel.FuncWriteDevToTun)

	go vpn.handler()
	vpn.handlerCtrC()

	log.Info("VPN started successfully!")
	log.Info("Version:", VERSION)

	for {
		if virtualChannel.TryNumber > MAX_TRY {
			log.Error("Failed to connect to server")
			break
		}
		err = virtualChannel.Run()
		log.Info(fmt.Sprintf("Try again(%d) in ", virtualChannel.TryNumber), TIME_TO_TRY, "...")
		time.Sleep(TIME_TO_TRY)
		err = virtualChannel.Connect(tokenUser, connectType)
		if err != nil {
			log.Error("connect vpn", err)
		}
	}

	return
}

func (vpn *VPN) handlerCtrC() {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		vpn.stop()
		os.Exit(1)
	}()
}

func (vpn *VPN) OnFuncWriteDevToTun(tunWrite func(c interface{}, data []byte) error) {
	vpn.writeDevToTun = func(header network.PacketHeader, data []byte) error {
		log.Debug("IPv6:", header.IsIPv6, "Src:", header.IPSrc.String(), "Dst:", header.IPDst.String(), string(data))

		r := vpn.getCurrentConnClient(header.IPDst.String())
		if r.Conn == nil {
			log.Debug("connection not found", header.IPDst)
			return nil
		}

		dataEn, err := crypto.AESEncrypt(r.Key, data)
		if err != nil {
			log.Debug("encrypt data error", err)
			return nil
		}

		return tunWrite(r.Conn, dataEn)
	}
}

func (vpn *VPN) writeTunToDev(key, data []byte) {
	rawData, err := crypto.AESDecrypt(key, data)
	if err != nil {
		log.Debug("decrypt data error", err)
		return
	}

	header := network.ParseHeaderPacket(rawData)
	if vpn.inMyNetwork(header.IPDst) {
		err = vpn.writeDevToTun(header, rawData)
		if err != nil {
			log.Debug("write dev to tun error", err)
		}
		return
	}

	_, err = vpn.dev.Write(rawData, 0)
	if err != nil {
		log.Error("write tun to dev err", err)
	}
}

func (vpn *VPN) handler() {
	buf := make([]byte, vpn.conf.MTU)
	for {
		n, err := vpn.dev.Read(buf, 0)
		if err != nil {
			log.Error("read data from vpn error", err)
			continue
		}
		packet := buf[:n]

		header := network.ParseHeaderPacket(packet)
		if vpn.blackList[header.IPDst.String()] {
			log.Debug("Block ip", header.IPDst)
			continue
		}

		err = vpn.writeDevToTun(header, packet)
		if err != nil {
			log.Debug("write dev to tun error", err)
			continue
		}

	}
}

func (self *VPN) authenConn(token string, conn interface{}) (string, []byte, func(id string)) {
	arr := strings.Split(token, ":")
	if len(arr) < 2 {
		return "", nil, nil
	}
	user := arr[0]
	u, found := self.userTable[user]
	if !found {
		return "", nil, nil
	}
	keyBase64 := arr[1]

	tokenByte, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return "", nil, nil
	}

	keyByte, err := crypto.AESDecrypt([]byte(u.Pass), tokenByte)
	if err != nil {
		return "", nil, nil
	}

	for _, c := range keyByte {
		if c < 48 || (58 < c && c < 64) || (91 < c && c < 96) || c > 123 {
			return "", nil, nil
		}
	}

	if !self.arpTable.Update(u.IP, conn, keyByte) {
		return u.IP, keyByte, self.arpTable.Delete
	}
	return "", nil, nil
}

func (vpn *VPN) setupAuthentication() {
	KEY_LEN := 32
	vpn.userTable = make(map[string]User, 0)

	for _, u := range vpn.conf.Users {
		pass := ""
		if len(u.Pass) < KEY_LEN {
			pass = fmt.Sprintf("%s%s", u.Pass, strings.Repeat("t", KEY_LEN-len(u.Pass)))
		}

		vpn.userTable[u.Name] = User{
			Pass: pass,
			IP:   network.GetIp(u.IP),
		}
	}
}

func (vpn *VPN) setupRoute() error {
	if YOUR_OS == "linux" {
		tunCmd := [][]string{
			{"link", "set", "dev", TUN_NAME, "mtu", fmt.Sprintf("%d", vpn.conf.MTU)},
			{"addr", "add", vpn.conf.LocalAddr, "dev", TUN_NAME},
			{"link", "set", "dev", TUN_NAME, "up"},
		}

		if !vpn.conf.IsServer {
			tunCmd = append(tunCmd, [][]string{
				{"route", "add", "0.0.0.0/1", "dev", TUN_NAME},
				{"route", "add", "128.0.0.0/1", "dev", TUN_NAME},
			}...)
		}

		for _, cmdAgrs := range tunCmd {
			err := runCmd("/sbin/ip", cmdAgrs...)
			if err != nil {
				return err
			}
		}
	} else if YOUR_OS == "windows" && !vpn.conf.IsServer {
		currentDefaultGateway, err := network.GetDefaultGatewayWindows()
		if err != nil {
			return err
		}

		iface, err := net.InterfaceByName(TUN_NAME)
		if err != nil {
			return err
		}

		vpn.conf.Whitelist = append(vpn.conf.Whitelist, network.GetIp(vpn.conf.ServerAddr)+"/32")

		tunCmd := [][]string{
			{"netsh", "interface", "ip", "set", "address", fmt.Sprintf("name=%d", iface.Index), "source=static", "addr=" + network.GetIp(vpn.conf.LocalAddr), "mask=" + network.CIDRToMask(vpn.conf.LocalAddr), "gateway=none"},
			{"route", "add", "0.0.0.0", "mask", "0.0.0.0", vpn.conf.DefaultGateway, "if", fmt.Sprintf("%d", iface.Index), "metric", "5"},
			// {"route", "add", network.GetIp(vpn.conf.ServerAddr), "mask", "255.255.255.255", currentDefaultGateway.Gateway},
		}

		for _, ipW := range vpn.conf.Whitelist {
			tunCmd = append(tunCmd, []string{
				"route", "add", network.GetIp(ipW), "mask", network.CIDRToMask(ipW), currentDefaultGateway.Gateway,
			})
		}

		for _, ipB := range vpn.conf.Blacklist {
			tunCmd = append(tunCmd, []string{
				"route", "add", ipB, "mask", "255.255.255.255", vpn.conf.DefaultGateway, "if", fmt.Sprintf("%d", iface.Index), "metric", "5",
			})
			vpn.blackList[ipB] = true
		}

		for _, cmdAgrs := range tunCmd {
			err := runCmd(cmdAgrs[0], cmdAgrs[1:]...)
			if err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("not support os: %v", YOUR_OS)
	}

	return nil
}

func (vpn *VPN) stop() {
	log.Info("Stop vpn ...")
	if vpn.conf.IsServer {
	} else {
		if YOUR_OS == "linux" {

		} else if YOUR_OS == "windows" {
			for _, ipW := range vpn.conf.Whitelist {
				err := runCmd("route", "delete", network.GetIp(ipW), "mask", network.CIDRToMask(ipW))
				if err != nil {
					log.Error(err)
				}
			}

			for _, ipB := range vpn.conf.Blacklist {
				err := runCmd("route", "delete", ipB)
				if err != nil {
					log.Error(err)
				}
			}
		}
	}
	log.Info("Done!(GoodBye)")
	// fmt.Println("Press the Enter Key to exit!")
	// fmt.Scanln()
}

func runCmd(c string, args ...string) error {
	log.Debug(c, strings.Join(args, " "))
	cmd := exec.Command(c, args...)
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("run cmd error: %v", err)
	}
	return err
}
