#调节音量
amixer -c1 sset 'aw_dev_0_rx_volume',0 300

## 一键部署（新音箱）
1. 复制配置模板：`cp deploy/ai_box.env.example deploy/ai_box.env` 并填写 `AI_BOX_DASH_API_KEY/WIFI` 等参数
2. 交叉编译或 docker 编译生成 `ai_box`
3. 上传到板子（示例）：`scp -r ai_box libluxaudio.so deploy root@<ip>:/userdata/AI_BOX/`
4. 板子上执行：`cd /userdata/AI_BOX && sh /userdata/AI_BOX/deploy/install.sh /userdata/AI_BOX/deploy/ai_box.env`
5. 之后可手动启动：`/userdata/AI_BOX/run.sh`（程序会自动读取 `/userdata/AI_BOX/ai_box.env`）





## Go语言交叉编译/docker编译
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ai_box main.go

----------------------------------------------

docker run --rm --platform linux/arm64 \
  -v "$PWD":/app -w /app \
  -e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=arm64 \
  -e CGO_CFLAGS="-I/app/sherpa_libs/include" \
  -e CGO_LDFLAGS="-L/app/sherpa_libs -lsherpa-onnx-c-api -lonnxruntime -lpthread -lm -lstdc++" \
  golang:1.23 go build -v -o ai_box .


## docker编译完之后检查文件最新生成时间
ls -lh ai_box


## 先删除再上传
ssh root@<ip> "rm -f /userdata/ai_box"
scp ai_box root@<ip>:/userdata/
scp -r ai_box sherpa_libs models keywords.txt root@<ip>:/userdata/

## 赋予权限
chmod +x /userdata/AI_BOX/deploy/install.sh




