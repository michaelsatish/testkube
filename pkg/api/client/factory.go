package client

type ClientType string

const (
	ClientDirect ClientType = "direct"
	ClientProxy  ClientType = "proxy"
)

func GetClient(clientType ClientType) (client Client, err error) {
	switch clientType {

	case ClientDirect:
		client = NewDafultDirectScriptsAPI()
	case ClientProxy:
		clientset, err := GetClientSet()
		if err != nil {
			return client, err
		}
		client = NewProxyScriptsAPI(clientset)
	}

	return client, err
}
