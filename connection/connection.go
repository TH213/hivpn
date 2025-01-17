package connection

type TUN struct {
	Addr              string
	HostHeader        string
	TryNumber         int
	Run               func() error
	FuncWriteTunToDev func(key, data []byte)
	FuncWriteDevToTun func(conn interface{}, data []byte) error
	FuncAuthenConn    func(token string, conn interface{}) (string, []byte, func(id string))
}

const (
	CONNECTION_TYPE_WEBSOCKET   = 1
	ERROR_AUTHENTICATION_FAILED = "Authentication failed"
)

func (self *TUN) Connect(token string, connectType int) error {
	switch connectType {
	case CONNECTION_TYPE_WEBSOCKET:
		self.TryNumber++
		srcConn, runFunc, err := self.createWebSocket(self.Addr, token)
		if err != nil {
			return err
		}

		srcConn.OnFuncWriteTunToDev(self.FuncWriteTunToDev)
		srcConn.OnAuthen(self.FuncAuthenConn)
		self.FuncWriteDevToTun = func(conn interface{}, data []byte) error {
			self.TryNumber = 0
			return srcConn.WriteDevToTun(conn, data)
		}

		self.onRun(runFunc)
	default:
	}
	return nil
}

func (t *TUN) onRun(f func() error) {
	t.Run = f
}
