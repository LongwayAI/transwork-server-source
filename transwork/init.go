package transwork

import twstorage "github.com/QuantumNous/new-api/transwork/storage"

func Init() error {
	return twstorage.InitGCSClient()
}
