# 测试记录

- 日期: 2026-01-23
- 执行者: Codex

## go test ./...

第一次执行（沙箱）:

```
FAIL	ai_box [setup failed]
FAIL	./... [setup failed]
FAIL
# ai_box
open /Users/zuki/Library/Caches/go-build/9d/9ddf69188a615a793276c1402c81e80e7ae9ebdcd21f0ba8d1675519c69bd7b1-d: operation not permitted
# ./...
pattern ./...: open /Users/zuki/Library/Caches/go-build/85/855d822ef7bdb15d481a29d21c85192c03797e8a12b41d460b064a7cd79af431-d: operation not permitted
```

第二次执行（已申请权限）:

```
ok  	ai_box	0.613s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第三次执行（已申请权限，更新过滤与随机逻辑后）:

```
ok  	ai_box	0.601s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第四次执行（已申请权限，更新播报与意图识别后）:

```
ok  	ai_box	0.596s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第五次执行（已申请权限，新增意图测试后）:

```
ok  	ai_box	0.574s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第六次执行（已申请权限，新增标题提取后）:

```
ok  	ai_box	0.605s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第七次执行（已申请权限，播放确认先播报后播放）:

```
ok  	ai_box	0.591s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第八次执行（已申请权限，扩展音乐意图识别）:

```
ok  	ai_box	0.649s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第九次执行（已申请权限，音乐意图去空格）:

```
ok  	ai_box	0.581s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第十次执行（已申请权限，TTS兜底过滤控制标记）:

```
ok  	ai_box	0.578s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第十一次执行（已申请权限，随机播放意图与打断调整）:

```
ok  	ai_box	0.584s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```

第十二次执行（已申请权限，打断强制丢弃）:

```
ok  	ai_box	0.693s
?   	ai_box/aec	[no test files]
?   	ai_box/vad	[no test files]
```
