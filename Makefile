travis:
	wget https://dl.bintray.com/mitchellh/consul/0.4.1_linux_amd64.zip
	unzip 0.4.1_linux_amd64.zip
	./consul agent -server -bootstrap -data-dir=tmp
	go test ./...