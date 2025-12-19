#调节音量
amixer -c1 sset 'aw_dev_0_rx_volume',0 300


#1.远程登陆板子
ssh root@10.110.4.210


#2.Go语言交叉编译
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ai_box main.go


#2.docker编译
docker run --rm --platform linux/arm64 \
  -v "$PWD":/app -w /app \
  -e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=arm64 \
  -e CGO_CFLAGS="-I/app/sherpa_libs/include" \
  -e CGO_LDFLAGS="-L/app/sherpa_libs -lsherpa-onnx-c-api -lonnxruntime -lpthread -lm -lstdc++" \
  golang:1.23 go build -v -o ai_box .


#3.docker编译完之后检查文件最新生成时间
ls -lh ai_box


#4.先删除再上传
ssh root@10.110.4.210 "rm -f /userdata/ai_box"
scp ai_box root@10.110.4.210:/userdata/
scp -r ai_box sherpa_libs models keywords.txt root@10.110.4.210:/userdata/

#5.赋予权限
chmod +x ai_box


#6.在板子上执行，设置环境变量
export LD_LIBRARY_PATH=$LD_LIBRARY_PATH:/userdata/



