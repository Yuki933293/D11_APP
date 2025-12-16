#1.Go语言交叉编译
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ai_box main.go

#2.先删除再上传
ssh root@10.110.4.210 "rm -f /userdata/ai_box"
scp ai_box root@10.110.4.210:/userdata/