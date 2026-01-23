package main

import "testing"

func TestControlTagFilter_Filter(t *testing.T) {
	filter := &controlTagFilter{}

	got := filter.Filter("好的，正在为您播放《庙堂之外》[PLAY: 庙堂之外]")
	if got != "好的，正在为您播放《庙堂之外》" {
		t.Fatalf("过滤控制标记失败，got=%q", got)
	}
}

func TestControlTagFilter_FilterSplit(t *testing.T) {
	filter := &controlTagFilter{}

	part1 := filter.Filter("好的，正在为您播放《庙堂之外》[")
	part2 := filter.Filter("PLAY: 庙堂之外]")
	got := part1 + part2
	if got != "好的，正在为您播放《庙堂之外》" {
		t.Fatalf("跨分片过滤失败，got=%q", got)
	}
}

func TestControlTagFilter_FilterDropAfterTag(t *testing.T) {
	filter := &controlTagFilter{}

	got := filter.Filter("好的，正在为您播放《庙堂之外》[PLAY: 庙堂之外]庙堂之外")
	if got != "好的，正在为您播放《庙堂之外》" {
		t.Fatalf("控制标记后仍输出文本，got=%q", got)
	}
}

func TestPickRandomExcluding(t *testing.T) {
	candidates := []string{"a.wav", "b.wav", "c.wav"}
	target, ok := pickRandomExcluding(candidates, "b.wav")
	if !ok {
		t.Fatalf("随机选择失败")
	}
	if target == "b.wav" {
		t.Fatalf("随机选择未排除当前曲目")
	}
}

func TestPickRandomExcludingSingle(t *testing.T) {
	candidates := []string{"a.wav"}
	target, ok := pickRandomExcluding(candidates, "a.wav")
	if !ok {
		t.Fatalf("单曲列表选择失败")
	}
	if target != "a.wav" {
		t.Fatalf("单曲列表返回异常，got=%q", target)
	}
}

func TestHasMusicIntent(t *testing.T) {
	if !hasMusicIntent("我想听歌") {
		t.Fatalf("音乐意图识别失败")
	}
	if !hasMusicIntent("放音乐") {
		t.Fatalf("音乐意图识别失败：放音乐")
	}
	if !hasMusicIntent("我 想 听 庙堂之外") {
		t.Fatalf("音乐意图识别失败：含空格")
	}
	if hasMusicIntent("今天天气怎么样") {
		t.Fatalf("误判为音乐意图")
	}
}

func TestNormalizeIntentText(t *testing.T) {
	got := normalizeIntentText("我 想 听 庙堂之外。")
	if got != "我想听庙堂之外" {
		t.Fatalf("意图文本清洗失败，got=%q", got)
	}
}

func TestIsRandomPlayIntent(t *testing.T) {
	if !isRandomPlayIntent("唱首歌") {
		t.Fatalf("随机播放意图识别失败：唱首歌")
	}
	if !isRandomPlayIntent("放音乐") {
		t.Fatalf("随机播放意图识别失败：放音乐")
	}
	if isRandomPlayIntent("不要放歌") {
		t.Fatalf("随机播放意图误判：否定表达")
	}
}

func TestExtractTitleFromPath(t *testing.T) {
	title := extractTitleFromPath("陈楚生《庙堂之外》2025-7-17.wav", "")
	if title != "庙堂之外" {
		t.Fatalf("标题提取失败，got=%q", title)
	}
	fallback := extractTitleFromPath("夏日游记 (Remastered).wav", "夏日游记")
	if fallback != "夏日游记 (Remastered)" {
		t.Fatalf("标题提取异常，got=%q", fallback)
	}
}
