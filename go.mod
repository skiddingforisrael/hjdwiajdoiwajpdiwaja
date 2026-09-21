module github.com/qaustria/AutoPack-Go

go 1.22

require (
	github.com/klauspost/compress v1.17.11
	github.com/robloxapi/rbxfile v0.6.5
	go.etcd.io/bbolt v1.3.11
)

require (
	github.com/anaminus/parse v0.2.0 // indirect
	github.com/bkaradzic/go-lz4 v1.0.0 // indirect
	golang.org/x/crypto v0.0.0-20210513164829-c07d793c2f9a // indirect
	golang.org/x/sys v0.4.0 // indirect
)

replace go.etcd.io/bbolt => github.com/etcd-io/bbolt v1.3.11

replace golang.org/x/crypto => github.com/golang/crypto v0.0.0-20210513164829-c07d793c2f9a

replace golang.org/x/sys => github.com/golang/sys v0.4.0
