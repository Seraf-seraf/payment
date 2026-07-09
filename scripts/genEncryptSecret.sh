MASTER_KEY='<MASTER_KEY>' \
SHARED_SECRET="$(openssl rand -base64 32)" \
MODULE="$(go list -m)" \
bash -c 'cat > /tmp/encryptsecret.go <<EOF
package main

import (
	"fmt"
	"os"

	appcrypto "$MODULE/pkg/crypto"
)

func main() {
	encrypted, err := appcrypto.EncryptSecret(os.Getenv("MASTER_KEY"), os.Getenv("SHARED_SECRET"))
	if err != nil {
		panic(err)
	}

	fmt.Println("SHARED_SECRET=" + os.Getenv("SHARED_SECRET"))
	fmt.Println("SHARED_SECRET_ENCRYPTED=" + encrypted)
}
EOF
go run /tmp/encryptsecret.go'