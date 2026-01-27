#调节音量
amixer -c1 sset 'aw_dev_0_rx_volume',0 300

## Python 版本构建与部署
1. 建议使用独立 conda 环境构建：
   - 创建并进入环境：`conda create -n ai_box_py python=3.11 -y && conda activate ai_box_py`
   - 安装构建工具：`python -m pip install pyinstaller`
2. macOS 上构建：`./scripts/build_mac.sh`
3. 若需要在 macOS 上构建 Linux/ARM64 版本（glibc），使用脚本：
   - `./scripts/build_linux_arm64.sh`
   - 说明：脚本固定使用 `python:3.11-slim-bookworm`（glibc 2.36），可在板子 glibc 2.37 上运行。
   - 说明：脚本内部会安装 `binutils`（提供 `objdump`），否则 PyInstaller 会失败。
3. 生成配置文件（本机）：`sh deploy/install_py.sh`（若不存在会生成 `deploy/ai_box.env`）
4. 上传到板子（示例）：`scp -r dist/ai_box_py libluxaudio.so deploy root@<ip>:/userdata/AI_BOX/`
5. 板子上执行：`cd /userdata/AI_BOX && sh /userdata/AI_BOX/deploy/install_py.sh /userdata/AI_BOX/deploy/ai_box.env`
6. 手动启动：`/userdata/AI_BOX/run_py.sh`
7. 若需要更高灵敏度 VAD，编译并上传 `libwebrtcvad.so`：
   - 构建：`./scripts/build_webrtcvad_arm64.sh`
   - 上传：`scp -r libwebrtcvad.so root@<ip>:/userdata/AI_BOX/`
   - Python 会自动加载 `/userdata/AI_BOX/libwebrtcvad.so`

## Python 版本重新编译（后续迭代）
1. 进入工程目录：`cd /Users/zuki/Downloads/D11_APP-Cloud-wake`
2. 激活构建环境：`conda activate ai_box_py`
3. 重新构建：`./scripts/build_mac.sh`

## Python 版本重新编译（后续迭代）
1. 进入工程目录：`cd /Users/zuki/Downloads/D11_APP-Cloud-wake`
2. 激活构建环境：`conda activate ai_box_py`
3. 重新构建：`./scripts/build_mac.sh`
