package windows

import (
	"context"
	"crypto/aes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/windows"

	"github.com/sjzar/chatlog/internal/wechat/decrypt"
	"github.com/sjzar/chatlog/internal/wechat/decrypt/common"
	"github.com/sjzar/chatlog/internal/wechat/model"
	"github.com/sjzar/chatlog/pkg/util/dat2img"
)

const (
	hexPatternLen = 96
	chunkSize     = 2 * 1024 * 1024
	chunkOverlap  = hexPatternLen + 3
)

type keyFileEntry struct {
	EncKey string `json:"enc_key"`
}

type dbSaltEntry struct {
	SaltHex string
	DBRel   string
}

type keySaltPair struct {
	KeyHex  string
	SaltHex string
}

type templateData struct {
	Ciphertext   []byte
	XorKey       *byte
	TemplateData []byte
}

func (e *V4Extractor) Extract(ctx context.Context, proc *model.Process) (string, string, error) {
	statusCB, _ := ctx.Value("status_callback").(func(string))
	imageOnly, _ := ctx.Value("image_key_only").(bool)
	forceRescan, _ := ctx.Value("force_rescan_memory").(bool)
	if proc == nil || proc.PID == 0 {
		return "", "", fmt.Errorf("windows 微信进程未就绪")
	}

	if proc.DataDir == "" {
		return "", "", fmt.Errorf("windows 数据目录未就绪，请确保微信已登录")
	}

	if !forceRescan {
		if statusCB != nil {
			statusCB("检查 all_keys.json...")
		}
		if key, err := loadAndValidateMessageKey(proc.DataDir, statusCB); err == nil && key != "" {
			if statusCB != nil {
				statusCB("已从 all_keys.json 获取密钥")
			}
			imgKey, err := e.pickImageKeyWithTiming(ctx, proc, statusCB, imageOnly)
			if err != nil {
				return "", "", err
			}
			return strings.ToLower(key), imgKey, nil
		}
	} else {
		if statusCB != nil {
			statusCB("已启用强制重扫：跳过旧 all_keys.json，重新扫描进程内存...")
		}
		_ = removeAllKeysFile(proc.DataDir)
	}

	if statusCB != nil {
		statusCB("开始 init 风格扫描：收集 DB salt -> 内存扫描 -> 写入 all_keys.json")
	}
	key, _, err := InitAllKeysByPID(proc.PID, proc.DataDir, statusCB)
	if err != nil {
		return "", "", err
	}
	if forceRescan && statusCB != nil {
		statusCB("本轮已完成内存重扫，all_keys.json 已更新，正在选取可用密钥...")
	}
	imgKey, err := e.pickImageKeyWithTiming(ctx, proc, statusCB, imageOnly)
	if err != nil {
		return "", "", err
	}
	return strings.ToLower(key), imgKey, nil
}

func (e *V4Extractor) pickImageKeyWithTiming(ctx context.Context, proc *model.Process, status func(string), imageOnly bool) (string, error) {
	if !imageOnly {
		return e.pickImageKeyWeFlow(proc.PID, proc.DataDir, status)
	}

	if status != nil {
		status("正在查找模板文件...")
	}
	resultTpl, ok := findTemplateData(proc.DataDir, 32)
	ciphertext := resultTpl.Ciphertext
	xorKey := resultTpl.XorKey
	if len(ciphertext) > 0 && xorKey == nil {
		if status != nil {
			status("未找到有效密钥，尝试扫描更多文件...")
		}
		resultTpl, ok = findTemplateData(proc.DataDir, 100)
		if ok {
			xorKey = resultTpl.XorKey
			ciphertext = resultTpl.Ciphertext
		}
	}
	if len(ciphertext) == 0 {
		return "", fmt.Errorf("未找到 V2 模板文件，请先在微信中查看几张图片")
	}
	if xorKey == nil {
		return "", fmt.Errorf("未能从模板文件中计算出有效的 XOR 密钥")
	}
	dat2img.V4XorKey = *xorKey
	if status != nil {
		status(fmt.Sprintf("XOR 密钥: 0x%02x，正在查找微信进程...", *xorKey))
	}
	if key, ok := deriveImageKeyByCodeAndWxid(proc.DataDir, status); ok {
		if status != nil {
			status("通过 kvcomm(code+wxid) 推导并验真成功")
		}
		return key, nil
	}

	deadline := time.Now().Add(60 * time.Second)
	scanRound := 0
	lastPID := uint32(0)
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("60 秒内未找到 AES 密钥")
		}

		currentPID, err := findWeChatPID()
		if err != nil || currentPID == 0 {
			if status != nil {
				status("暂未检测到微信主进程，请确认微信已经重新打开...")
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		}
		if currentPID != lastPID {
			lastPID = currentPID
			if status != nil {
				status(fmt.Sprintf("已找到微信进程 PID=%d，正在扫描内存...", currentPID))
			}
		}

		scanRound++
		if status != nil {
			status(fmt.Sprintf("第 %d 次扫描内存，请在微信中打开图片大图...", scanRound))
		}

		imgKey, checked, err := scanImageKeyByWeFlow(currentPID, ciphertext, resultTpl.TemplateData)
		if err != nil {
			log.Debug().Err(err).Msg("扫描图片密钥失败，准备重试")
		}
		if status != nil {
			status(fmt.Sprintf("正在扫描图片密钥... 已检查 %d 个候选字符串", checked))
		}
		if imgKey != "" {
			if status != nil {
				status(fmt.Sprintf("通过字符串扫描找到图片密钥! (在检查了 %d 个候选后)", checked))
			}
			return imgKey, nil
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (e *V4Extractor) pickImageKeyWeFlow(pid uint32, dataDir string, status func(string)) (string, error) {
	tpl, ok := findTemplateData(dataDir, 32)
	if !ok || len(tpl.Ciphertext) == 0 || tpl.XorKey == nil {
		tpl, ok = findTemplateData(dataDir, 100)
	}
	if !ok || len(tpl.Ciphertext) == 0 || tpl.XorKey == nil {
		return "", nil
	}
	dat2img.V4XorKey = *tpl.XorKey
	if key, ok := deriveImageKeyByCodeAndWxid(dataDir, status); ok {
		if status != nil {
			status("通过 kvcomm(code+wxid) 推导并验真成功")
		}
		return key, nil
	}
	imgKey, checked, err := scanImageKeyByWeFlow(pid, tpl.Ciphertext, tpl.TemplateData)
	if status != nil {
		status(fmt.Sprintf("正在扫描图片密钥... 已检查 %d 个候选字符串", checked))
	}
	if err != nil {
		return "", err
	}
	if imgKey != "" && status != nil {
		status(fmt.Sprintf("通过字符串扫描找到图片密钥! (在检查了 %d 个候选后)", checked))
	}
	return imgKey, nil
}

func scanImageKeyByWeFlow(pid uint32, ciphertext []byte, templateDat []byte) (string, int, error) {
	if key, checked, err := scanImageKeyByPIDAndCiphertext(pid, ciphertext); err == nil || key != "" {
		if key != "" && verifyImageAesKeyStrong([]byte(key), templateDat) {
			return key, checked, nil
		}
	}

	cands, checked, err := scanImageKeyCandidatesByPID(pid)
	if err != nil {
		return "", checked, err
	}
	for _, c := range cands {
		if len(c) < 16 {
			continue
		}
		k := c[:16]
		if verifyImageAesKeyWeFlow([]byte(k), ciphertext) && verifyImageAesKeyStrong([]byte(k), templateDat) {
			return k, checked, nil
		}
	}
	any16, checkedAny16, err := scanImageAny16CandidatesByPID(pid)
	checked += checkedAny16
	if err != nil {
		return "", checked, nil
	}
	for _, k := range any16 {
		if verifyImageAesKeyWeFlow([]byte(k), ciphertext) && verifyImageAesKeyStrong([]byte(k), templateDat) {
			return k, checked, nil
		}
	}
	return "", checked, nil
}

func verifyImageAesKeyStrong(aes16 []byte, templateDat []byte) bool {
	if len(aes16) != aes.BlockSize || len(templateDat) < 15 {
		return false
	}
	_, _, err := dat2img.Dat2ImageV4(templateDat, aes16)
	return err == nil
}

func verifyImageAesKeyWeFlow(aes16 []byte, ciphertext []byte) bool {
	if len(aes16) != 16 || len(ciphertext) != 16 {
		return false
	}
	block, err := aes.NewCipher(aes16)
	if err != nil {
		return false
	}
	out := make([]byte, 16)
	block.Decrypt(out, ciphertext)

	if out[0] == 0xFF && out[1] == 0xD8 && out[2] == 0xFF {
		return true
	}
	if out[0] == 0x89 && out[1] == 0x50 && out[2] == 0x4E && out[3] == 0x47 {
		return true
	}
	if out[0] == 0x52 && out[1] == 0x49 && out[2] == 0x46 && out[3] == 0x46 {
		return true
	}
	if out[0] == 0x77 && out[1] == 0x78 && out[2] == 0x67 && out[3] == 0x66 {
		return true
	}
	if out[0] == 0x47 && out[1] == 0x49 && out[2] == 0x46 {
		return true
	}
	return false
}

func findTemplateData(dataDir string, limit int) (templateData, bool) {
	const sampleOffset = 0x0F
	const sampleLen = 16
	magic := []byte{0x07, 0x08, 0x56, 0x32, 0x08, 0x07}

	files := make([]string, 0, limit)
	_ = filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), "_t.dat") {
			files = append(files, path)
		}
		return nil
	})
	if len(files) == 0 {
		return templateData{}, false
	}
	sort.Slice(files, func(i, j int) bool {
		ai, erri := os.Stat(files[i])
		aj, errj := os.Stat(files[j])
		if erri != nil || errj != nil {
			return files[i] < files[j]
		}
		return ai.ModTime().After(aj.ModTime())
	})
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}

	var ciphertext []byte
	var templateRaw []byte
	tailCounts := map[string]int{}

	maxProbe := 32
	if len(files) < maxProbe {
		maxProbe = len(files)
	}
	for _, f := range files[:maxProbe] {
		b, err := os.ReadFile(f)
		if err != nil || len(b) < 8 {
			continue
		}
		if len(b) >= 6 && bytesEqual(b[:6], magic) {
			if len(b) >= sampleOffset+sampleLen && ciphertext == nil {
				ciphertext = make([]byte, sampleLen)
				copy(ciphertext, b[sampleOffset:sampleOffset+sampleLen])
				templateRaw = make([]byte, len(b))
				copy(templateRaw, b)
			}
			key := fmt.Sprintf("%d_%d", b[len(b)-2], b[len(b)-1])
			tailCounts[key]++
		}
	}
	if ciphertext == nil {
		return templateData{}, false
	}

	var (
		bestCount int
		xorKey    *byte
	)
	for k, count := range tailCounts {
		if count <= bestCount {
			continue
		}
		parts := strings.Split(k, "_")
		if len(parts) != 2 {
			continue
		}
		x, errX := strconv.Atoi(parts[0])
		y, errY := strconv.Atoi(parts[1])
		if errX != nil || errY != nil {
			continue
		}
		kv := byte(x) ^ 0xFF
		if kv == (byte(y) ^ 0xD9) {
			tmp := kv
			xorKey = &tmp
			bestCount = count
		}
	}
	return templateData{Ciphertext: ciphertext, XorKey: xorKey, TemplateData: templateRaw}, true
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func InitAllKeysByPID(pid uint32, dataDir string, status func(string)) (string, int, error) {
	if pid == 0 {
		return "", 0, fmt.Errorf("invalid pid")
	}
	if dataDir == "" {
		return "", 0, fmt.Errorf("invalid dataDir")
	}

	accountDir, dbStorageDir := resolveDBDirs(dataDir)
	dbSalts, err := collectDBSalts(dbStorageDir)
	if err != nil {
		return "", 0, err
	}
	if len(dbSalts) == 0 {
		return "", 0, fmt.Errorf("未找到可用加密数据库（db_storage）")
	}
	if status != nil {
		status(fmt.Sprintf("已收集加密数据库 salt：%d 个", len(dbSalts)))
	}

	pairs, err := scanKeySaltPairsByPID(pid)
	if err != nil {
		return "", 0, err
	}
	if len(pairs) == 0 {
		return "", 0, fmt.Errorf("内存扫描未发现候选 key/salt")
	}
	if status != nil {
		status(fmt.Sprintf("内存扫描完成：候选 key/salt %d 组", len(pairs)))
	}

	out := map[string]keyFileEntry{}
	for _, pair := range pairs {
		for _, ds := range dbSalts {
			if pair.SaltHex != ds.SaltHex {
				continue
			}
			if _, exists := out[ds.DBRel]; !exists {
				out[ds.DBRel] = keyFileEntry{EncKey: strings.ToLower(pair.KeyHex)}
			}
		}
	}
	if len(out) == 0 {
		return "", 0, fmt.Errorf("扫描到候选 key，但未匹配到任意数据库 salt")
	}

	keysPath := filepath.Join(accountDir, "all_keys.json")
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", 0, fmt.Errorf("序列化 all_keys.json 失败: %w", err)
	}
	if err := os.WriteFile(keysPath, raw, 0600); err != nil {
		return "", 0, fmt.Errorf("写入 %s 失败: %w", keysPath, err)
	}
	if status != nil {
		status(fmt.Sprintf("已写入 all_keys.json：%s（%d 条）", keysPath, len(out)))
	}

	key, err := loadAndValidateMessageKey(accountDir, status)
	if err != nil {
		return "", len(out), err
	}
	return key, len(out), nil
}

func removeAllKeysFile(dataDir string) error {
	accountDir, _ := resolveDBDirs(dataDir)
	paths := []string{filepath.Join(accountDir, "all_keys.json"), filepath.Join(dataDir, "all_keys.json")}
	var lastErr error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			lastErr = err
		}
	}
	return lastErr
}

func loadAndValidateMessageKey(dataDir string, status func(string)) (string, error) {
	keys, err := loadAllKeys(dataDir)
	if err != nil {
		return "", err
	}
	if status != nil {
		status(fmt.Sprintf("检查 all_keys.json（共 %d 条）...", len(keys)))
	}
	if key, ok := pickPreferredMessageKey(dataDir, keys, status); ok {
		if status != nil {
			status("已从 all_keys.json 选中可用密钥")
		}
		return key, nil
	}
	return "", fmt.Errorf("all_keys.json 中没有有效 enc_key")
}

func loadAllKeys(dataDir string) (map[string]string, error) {
	candidates := []string{filepath.Join(dataDir, "all_keys.json"), "all_keys.json"}
	if strings.EqualFold(filepath.Base(filepath.Clean(dataDir)), "db_storage") {
		candidates = append([]string{filepath.Join(filepath.Dir(filepath.Clean(dataDir)), "all_keys.json")}, candidates...)
	}

	var content []byte
	var used string
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err == nil {
			content = b
			used = p
			break
		}
	}
	if used == "" {
		return nil, fmt.Errorf("未找到 all_keys.json（请先获取数据库密钥）")
	}

	obj := map[string]any{}
	if err := json.Unmarshal(content, &obj); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", used, err)
	}
	if len(obj) == 0 {
		return nil, fmt.Errorf("%s 为空", used)
	}

	out := make(map[string]string, len(obj))
	for dbPath, raw := range obj {
		key := ""
		switch v := raw.(type) {
		case string:
			key = v
		case map[string]any:
			if vv, ok := v["enc_key"].(string); ok {
				key = vv
			}
		}
		key = strings.TrimSpace(strings.ToLower(key))
		if len(key) != 64 {
			continue
		}
		out[normalizePath(dbPath)] = key
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 中没有有效 enc_key", used)
	}
	return out, nil
}

func pickPreferredMessageKey(dataDir string, keys map[string]string, status func(string)) (string, bool) {
	if len(keys) == 0 {
		return "", false
	}
	for dbRel, key := range keys {
		p := normalizePath(dbRel)
		if p == "message/message_0.db" || strings.HasSuffix(p, "/message/message_0.db") {
			if validateKeyOnDBPath(dataDir, dbRel, key) {
				return strings.ToLower(key), true
			}
		}
	}
	for dbRel, key := range keys {
		p := normalizePath(dbRel)
		if (strings.Contains(p, "/message/") || strings.HasPrefix(p, "message/")) && strings.HasSuffix(p, ".db") {
			if validateKeyOnDBPath(dataDir, dbRel, key) {
				return strings.ToLower(key), true
			}
		}
	}

	type keyCount struct {
		Key   string
		Count int
	}
	counter := map[string]int{}
	for _, key := range keys {
		k := strings.TrimSpace(strings.ToLower(key))
		if len(k) == 64 {
			counter[k]++
		}
	}
	if len(counter) == 0 {
		return "", false
	}
	counts := make([]keyCount, 0, len(counter))
	for k, c := range counter {
		counts = append(counts, keyCount{Key: k, Count: c})
	}
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Count == counts[j].Count {
			return counts[i].Key < counts[j].Key
		}
		return counts[i].Count > counts[j].Count
	})
	if status != nil {
		status(fmt.Sprintf("message库未命中，按频次回退选择候选 key（top=%d）", counts[0].Count))
	}
	return counts[0].Key, true
}

func validateKeyOnDBPath(dataDir, dbRelPath, keyHex string) bool {
	keyHex = strings.TrimSpace(strings.ToLower(keyHex))
	if len(keyHex) != 64 {
		return false
	}
	keyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return false
	}
	dbPath := resolveDBPath(dataDir, dbRelPath)
	dbInfo, err := common.OpenDBFile(dbPath, 4096)
	if err != nil {
		return false
	}
	d, err := decrypt.NewDecryptor(model.PlatformWindows, 4)
	if err != nil {
		return false
	}
	return d.Validate(dbInfo.FirstPage, keyBytes)
}

func resolveDBDirs(dataDir string) (accountDir string, dbStorageDir string) {
	clean := filepath.Clean(dataDir)
	if clean == "." || clean == "" {
		return dataDir, filepath.Join(dataDir, "db_storage")
	}
	base := strings.ToLower(filepath.Base(clean))
	if base == "db_storage" {
		return filepath.Dir(clean), clean
	}
	return clean, filepath.Join(clean, "db_storage")
}

func resolveDBPath(dataDir, dbRelPath string) string {
	_, dbStorageDir := resolveDBDirs(dataDir)
	p := normalizePath(dbRelPath)
	if filepath.IsAbs(dbRelPath) {
		return dbRelPath
	}
	if strings.HasPrefix(p, "db_storage/") {
		return filepath.Join(filepath.Dir(dbStorageDir), filepath.FromSlash(p))
	}
	return filepath.Join(dbStorageDir, filepath.FromSlash(p))
}

func collectDBSalts(dbStorageDir string) ([]dbSaltEntry, error) {
	stat, err := os.Stat(dbStorageDir)
	if err != nil || !stat.IsDir() {
		return nil, fmt.Errorf("数据库目录不存在: %s", dbStorageDir)
	}
	out := make([]dbSaltEntry, 0, 64)
	err = filepath.WalkDir(dbStorageDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(path), ".db") {
			return nil
		}
		salt, ok := readDBSalt(path)
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(dbStorageDir, path)
		if err != nil {
			return nil
		}
		out = append(out, dbSaltEntry{SaltHex: salt, DBRel: normalizePath(rel)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func readDBSalt(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, 16)
	if _, err := io.ReadFull(f, buf); err != nil {
		return "", false
	}
	if string(buf[:15]) == "SQLite format 3" {
		return "", false
	}
	return strings.ToLower(hex.EncodeToString(buf)), true
}

func scanKeySaltPairsByPID(pid uint32) ([]keySaltPair, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_VM_READ|windows.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		return nil, fmt.Errorf("open process failed: %w", err)
	}
	defer windows.CloseHandle(handle)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return scanKeySaltCandidates(ctx, handle)
}

func scanKeySaltCandidates(ctx context.Context, handle windows.Handle) ([]keySaltPair, error) {
	results := make([]keySaltPair, 0, 128)
	seen := make(map[string]struct{})
	var addr uintptr
	for {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}
		var mbi windows.MemoryBasicInformation
		err := windows.VirtualQueryEx(handle, addr, &mbi, unsafe.Sizeof(mbi))
		if err != nil {
			break
		}
		if mbi.State == windows.MEM_COMMIT && isRWProtect(mbi.Protect) && mbi.RegionSize > 0 {
			scanRegionForKeySalt(ctx, handle, uintptr(mbi.BaseAddress), uintptr(mbi.RegionSize), &results, seen)
		}
		next := uintptr(mbi.BaseAddress) + uintptr(mbi.RegionSize)
		if next <= addr {
			break
		}
		addr = next
	}
	return results, nil
}

func scanRegionForKeySalt(ctx context.Context, handle windows.Handle, base, size uintptr, out *[]keySaltPair, seen map[string]struct{}) {
	for offset := uintptr(0); offset < size; {
		select {
		case <-ctx.Done():
			return
		default:
		}
		left := size - offset
		readSize := uintptr(chunkSize)
		if left < readSize {
			readSize = left
		}
		buf := make([]byte, readSize)
		var bytesRead uintptr
		if err := windows.ReadProcessMemory(handle, base+offset, &buf[0], readSize, &bytesRead); err == nil && bytesRead > 0 {
			searchKeySaltPattern(buf[:bytesRead], out, seen)
		}
		if readSize > chunkOverlap {
			offset += readSize - chunkOverlap
		} else {
			offset += readSize
		}
	}
}

func searchKeySaltPattern(buf []byte, out *[]keySaltPair, seen map[string]struct{}) {
	total := hexPatternLen + 3
	if len(buf) < total {
		return
	}
	for i := 0; i+total <= len(buf); i++ {
		if buf[i] != 'x' || buf[i+1] != '\'' {
			continue
		}
		hexStart := i + 2
		valid := true
		for j := 0; j < hexPatternLen; j++ {
			if !isHexByte(buf[hexStart+j]) {
				valid = false
				break
			}
		}
		if !valid || buf[hexStart+hexPatternLen] != '\'' {
			continue
		}
		keyHex := strings.ToLower(string(buf[hexStart : hexStart+64]))
		saltHex := strings.ToLower(string(buf[hexStart+64 : hexStart+96]))
		id := keyHex + ":" + saltHex
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		*out = append(*out, keySaltPair{KeyHex: keyHex, SaltHex: saltHex})
	}
}

func scanImageKeyByPIDAndCiphertext(pid uint32, ciphertext []byte) (string, int, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_VM_READ|windows.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		return "", 0, fmt.Errorf("open process failed: %w", err)
	}
	defer windows.CloseHandle(handle)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return scanImageKeyByPIDAndCiphertextHandle(ctx, handle, ciphertext)
}

func scanImageKeyByPIDAndCiphertextHandle(ctx context.Context, handle windows.Handle, ciphertext []byte) (string, int, error) {
	if len(ciphertext) != 16 {
		return "", 0, fmt.Errorf("invalid ciphertext length: %d", len(ciphertext))
	}
	checked := 0
	var addr uintptr
	for {
		select {
		case <-ctx.Done():
			return "", checked, ctx.Err()
		default:
		}
		var mbi windows.MemoryBasicInformation
		err := windows.VirtualQueryEx(handle, addr, &mbi, unsafe.Sizeof(mbi))
		if err != nil {
			break
		}
		if mbi.State == windows.MEM_COMMIT && isRWProtect(mbi.Protect) && mbi.RegionSize > 0 {
			if key := searchImageKeyInRegion(ctx, handle, uintptr(mbi.BaseAddress), uintptr(mbi.RegionSize), ciphertext, &checked); key != "" {
				return key, checked, nil
			}
		}
		next := uintptr(mbi.BaseAddress) + uintptr(mbi.RegionSize)
		if next <= addr {
			break
		}
		addr = next
	}
	return "", checked, nil
}

func searchImageKeyInRegion(ctx context.Context, handle windows.Handle, base, size uintptr, ciphertext []byte, checked *int) string {
	var trailing []byte
	for offset := uintptr(0); offset < size; {
		select {
		case <-ctx.Done():
			return ""
		default:
		}
		left := size - offset
		readSize := uintptr(4 * 1024 * 1024)
		if left < readSize {
			readSize = left
		}
		buf := make([]byte, readSize)
		var bytesRead uintptr
		if err := windows.ReadProcessMemory(handle, base+offset, &buf[0], readSize, &bytesRead); err == nil && bytesRead > 0 {
			data := buf[:bytesRead]
			if len(trailing) > 0 {
				merged := make([]byte, 0, len(trailing)+len(data))
				merged = append(merged, trailing...)
				merged = append(merged, data...)
				data = merged
			}
			if key := searchAsciiKey(data, ciphertext, checked); key != "" {
				return key
			}
			if key := searchUTF16Key(data, ciphertext, checked); key != "" {
				return key
			}
			if len(data) > 65 {
				trailing = append([]byte(nil), data[len(data)-65:]...)
			} else {
				trailing = append([]byte(nil), data...)
			}
		} else {
			trailing = nil
		}
		offset += readSize
	}
	return ""
}

func searchAsciiKey(data []byte, ciphertext []byte, checked *int) string {
	for i := 0; i+34 <= len(data); i++ {
		if isAlphaNum(data[i]) {
			continue
		}
		valid := true
		for j := 1; j <= 32; j++ {
			if !isAlphaNum(data[i+j]) {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		if i+33 < len(data) && isAlphaNum(data[i+33]) {
			continue
		}
		*checked++
		k := string(data[i+1 : i+33])
		if len(k) < 16 {
			continue
		}
		if verifyImageAesKeyWeFlow([]byte(k[:16]), ciphertext) {
			return k[:16]
		}
	}
	return ""
}

func searchUTF16Key(data []byte, ciphertext []byte, checked *int) string {
	for i := 0; i+64 <= len(data); i++ {
		valid := true
		keyBytes := make([]byte, 32)
		for j := 0; j < 32; j++ {
			c := data[i+j*2]
			if data[i+j*2+1] != 0x00 || !isAlphaNum(c) {
				valid = false
				break
			}
			keyBytes[j] = c
		}
		if !valid {
			continue
		}
		*checked++
		k := string(keyBytes)
		if verifyImageAesKeyWeFlow([]byte(k[:16]), ciphertext) {
			return k[:16]
		}
	}
	return ""
}

func scanImageKeyCandidatesByPID(pid uint32) ([]string, int, error) {
	return scanAlphaNumCandidatesByPID(pid, 32)
}

func scanImageAny16CandidatesByPID(pid uint32) ([]string, int, error) {
	return scanAlphaNumCandidatesByPID(pid, 16)
}

func scanAlphaNumCandidatesByPID(pid uint32, n int) ([]string, int, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_VM_READ|windows.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		return nil, 0, fmt.Errorf("open process failed: %w", err)
	}
	defer windows.CloseHandle(handle)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	seen := map[string]struct{}{}
	checked := 0
	var addr uintptr
	for {
		select {
		case <-ctx.Done():
			keys := make([]string, 0, len(seen))
			for k := range seen {
				keys = append(keys, k)
			}
			return keys, checked, nil
		default:
		}
		var mbi windows.MemoryBasicInformation
		err := windows.VirtualQueryEx(handle, addr, &mbi, unsafe.Sizeof(mbi))
		if err != nil {
			break
		}
		if mbi.State == windows.MEM_COMMIT && isRWProtect(mbi.Protect) && mbi.RegionSize > 0 {
			collectCandidatesInRegion(ctx, handle, uintptr(mbi.BaseAddress), uintptr(mbi.RegionSize), n, seen, &checked)
		}
		next := uintptr(mbi.BaseAddress) + uintptr(mbi.RegionSize)
		if next <= addr {
			break
		}
		addr = next
	}

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	return keys, checked, nil
}

func collectCandidatesInRegion(ctx context.Context, handle windows.Handle, base, size uintptr, n int, seen map[string]struct{}, checked *int) {
	var trailing []byte
	overlap := uintptr(n*2 + 2)
	for offset := uintptr(0); offset < size; {
		select {
		case <-ctx.Done():
			return
		default:
		}
		left := size - offset
		readSize := uintptr(4 * 1024 * 1024)
		if left < readSize {
			readSize = left
		}
		buf := make([]byte, readSize)
		var bytesRead uintptr
		if err := windows.ReadProcessMemory(handle, base+offset, &buf[0], readSize, &bytesRead); err == nil && bytesRead > 0 {
			data := buf[:bytesRead]
			if len(trailing) > 0 {
				merged := make([]byte, 0, len(trailing)+len(data))
				merged = append(merged, trailing...)
				merged = append(merged, data...)
				data = merged
			}
			collectAsciiCandidates(data, n, seen, checked)
			collectUTF16Candidates(data, n, seen, checked)
			if len(data) > int(overlap) {
				trailing = append([]byte(nil), data[len(data)-int(overlap):]...)
			} else {
				trailing = append([]byte(nil), data...)
			}
		} else {
			trailing = nil
		}
		offset += readSize
	}
}

func collectAsciiCandidates(data []byte, n int, seen map[string]struct{}, checked *int) {
	if len(data) < n {
		return
	}
	for i := 0; i+n <= len(data); i++ {
		if i > 0 && isAlphaNum(data[i-1]) {
			continue
		}
		valid := true
		for j := 0; j < n; j++ {
			if !isAlphaNum(data[i+j]) {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		if i+n < len(data) && isAlphaNum(data[i+n]) {
			continue
		}
		*checked++
		seen[string(data[i:i+n])] = struct{}{}
	}
}

func collectUTF16Candidates(data []byte, n int, seen map[string]struct{}, checked *int) {
	need := n * 2
	if len(data) < need {
		return
	}
	for i := 0; i+need <= len(data); i++ {
		valid := true
		out := make([]byte, n)
		for j := 0; j < n; j++ {
			c := data[i+j*2]
			if data[i+j*2+1] != 0x00 || !isAlphaNum(c) {
				valid = false
				break
			}
			out[j] = c
		}
		if !valid {
			continue
		}
		*checked++
		seen[string(out)] = struct{}{}
	}
}

func isAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isRWProtect(p uint32) bool {
	const (
		pageReadWrite     = 0x04
		pageWriteCopy     = 0x08
		pageExecReadWrite = 0x40
		pageExecWriteCopy = 0x80
		pageGuard         = 0x100
		pageNoAccess      = 0x01
	)
	if p == pageNoAccess || (p&pageGuard) != 0 {
		return false
	}
	return (p&pageReadWrite) != 0 || (p&pageWriteCopy) != 0 || (p&pageExecReadWrite) != 0 || (p&pageExecWriteCopy) != 0
}

func isHexByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func findWeChatPID() (uint32, error) {
	procs, err := process.Processes()
	if err != nil {
		return 0, err
	}
	var maxPID uint32
	for _, p := range procs {
		name, err := p.Name()
		if err != nil {
			continue
		}
		name = strings.TrimSuffix(strings.ToLower(name), ".exe")
		if name != "weixin" && name != "wechat" {
			continue
		}
		if name == "weixin" {
			if cmd, err := p.Cmdline(); err == nil {
				if strings.Contains(cmd, "--") {
					continue
				}
			}
		}
		pid := uint32(p.Pid)
		if pid > maxPID {
			maxPID = pid
		}
	}
	if maxPID == 0 {
		return 0, fmt.Errorf("wechat process not found")
	}
	return maxPID, nil
}

func normalizePath(p string) string {
	return strings.TrimPrefix(strings.ToLower(strings.ReplaceAll(filepath.ToSlash(filepath.Clean(p)), "\\", "/")), "./")
}

func isIgnoredAccountName(v string) bool {
	lv := strings.ToLower(strings.TrimSpace(v))
	switch lv {
	case "", "xwechat_files", "wechat files", "all_users", "backup", "wmpf", "app_data":
		return true
	default:
		return false
	}
}

func isReasonableAccountID(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.Contains(v, "/") || strings.Contains(v, "\\") {
		return false
	}
	return !isIgnoredAccountName(v)
}

func normalizeAccountID(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	lv := strings.ToLower(v)
	if strings.HasPrefix(lv, "wxid_") {
		if idx := strings.Index(v[5:], "_"); idx >= 0 {
			return v[:5+idx]
		}
		return v
	}
	if m := regexp.MustCompile(`^(.+)_([a-zA-Z0-9]{4})$`).FindStringSubmatch(v); len(m) == 3 {
		return m[1]
	}
	return v
}

func pushAccountIDCandidate(out *[]string, v string) {
	push := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, e := range *out {
			if e == s {
				return
			}
		}
		*out = append(*out, s)
	}
	if !isReasonableAccountID(v) {
		return
	}
	push(v)
	nv := normalizeAccountID(v)
	if nv != "" && nv != v && isReasonableAccountID(nv) {
		push(nv)
	}
}

func collectWxidCandidates(dataDir string) []string {
	out := make([]string, 0, 8)
	pushAccountIDCandidate(&out, filepath.Base(filepath.Clean(dataDir)))

	if root, ok := accountRootDir(dataDir); ok {
		entries, err := os.ReadDir(root)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				pushAccountIDCandidate(&out, e.Name())
			}
		}
	}
	if len(out) == 0 {
		pushAccountIDCandidate(&out, "unknown")
	}
	return out
}

func isAccountDirPath(dir string) bool {
	if dir == "" {
		return false
	}
	checks := []string{
		filepath.Join(dir, "db_storage"),
		filepath.Join(dir, "msg"),
		filepath.Join(dir, "FileStorage", "Image"),
		filepath.Join(dir, "FileStorage", "Image2"),
	}
	for _, p := range checks {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

func collectAccountPathCandidates(dataDir string) []string {
	out := make([]string, 0, 8)
	uniqueAppendPath(&out, dataDir)

	if root, ok := accountRootDir(dataDir); ok {
		entries, err := os.ReadDir(root)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() || !isReasonableAccountID(e.Name()) {
					continue
				}
				entryPath := filepath.Join(root, e.Name())
				if !isAccountDirPath(entryPath) {
					continue
				}
				uniqueAppendPath(&out, entryPath)
			}
		}
	}
	return out
}

func accountRootDir(dataDir string) (string, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(strings.TrimRight(dataDir, "\\/")), "\\", "/")
	if normalized == "" {
		return "", false
	}
	lower := strings.ToLower(normalized)
	for _, marker := range []string{"/xwechat_files", "/wechat files"} {
		if idx := strings.Index(lower, marker); idx >= 0 {
			root := filepath.FromSlash(normalized[:idx+len(marker)])
			if st, err := os.Stat(root); err == nil && st.IsDir() {
				return root, true
			}
		}
	}
	return "", false
}

func uniqueAppendPath(out *[]string, v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	for _, e := range *out {
		if e == v {
			return
		}
	}
	*out = append(*out, v)
}

func getKvcommCandidates(dataDir string) []string {
	out := make([]string, 0, 16)
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		uniqueAppendPath(&out, filepath.Join(u.HomeDir, "AppData", "Roaming", "Tencent", "xwechat_files", "app_data", "net", "kvcomm"))
	}
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		uniqueAppendPath(&out, filepath.Join(local, "Tencent", "xwechat_files", "app_data", "net", "kvcomm"))
		uniqueAppendPath(&out, filepath.Join(local, "Tencent", "WeChat", "xwechat", "net", "kvcomm"))
	}
	if dataDir != "" {
		normalized := strings.ReplaceAll(strings.TrimRight(dataDir, "\\/"), "\\", "/")
		if idx := strings.Index(strings.ToLower(normalized), "/xwechat_files"); idx >= 0 {
			base := normalized[:idx]
			uniqueAppendPath(&out, filepath.FromSlash(base+"/app_data/net/kvcomm"))
		}
		if idx := strings.Index(strings.ToLower(normalized), "/wechat files"); idx >= 0 {
			base := normalized[:idx]
			uniqueAppendPath(&out, filepath.FromSlash(base+"/app_data/net/kvcomm"))
		}
		cursor := dataDir
		for i := 0; i < 6; i++ {
			uniqueAppendPath(&out, filepath.Join(cursor, "net", "kvcomm"))
			next := filepath.Dir(cursor)
			if next == cursor {
				break
			}
			cursor = next
		}
	}
	return out
}

func collectKvcommCodes(dataDir string) []int {
	codeSet := map[int]struct{}{}
	pat := regexp.MustCompile(`^key_(\d+)_.+\.statistic$`)
	for _, dir := range getKvcommCandidates(dataDir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			m := pat.FindStringSubmatch(e.Name())
			if len(m) != 2 {
				continue
			}
			code64, err := strconv.ParseUint(m[1], 10, 32)
			if err != nil || code64 == 0 {
				continue
			}
			codeSet[int(code64)] = struct{}{}
		}
	}
	if len(codeSet) == 0 {
		return nil
	}
	out := make([]int, 0, len(codeSet))
	for k := range codeSet {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func deriveImageKeyFromCodeWxid(code int, wxid string) (byte, string) {
	cleaned := normalizeAccountID(wxid)
	xorKey := byte(code & 0xFF)
	sum := md5.Sum([]byte(strconv.Itoa(code) + cleaned))
	aesKey := hex.EncodeToString(sum[:])[:16]
	return xorKey, aesKey
}

func deriveImageKeyByCodeAndWxid(dataDir string, status func(string)) (string, bool) {
	codes := collectKvcommCodes(dataDir)
	wxids := collectWxidCandidates(dataDir)
	accountPaths := collectAccountPathCandidates(dataDir)
	if status != nil {
		status(fmt.Sprintf("正在校验 code+wxid 组合... code=%d, wxid=%d, account=%d", len(codes), len(wxids), len(accountPaths)))
	}
	if len(codes) == 0 || len(wxids) == 0 {
		return "", false
	}

	for _, accountPath := range accountPaths {
		tpl, ok := findTemplateData(accountPath, 32)
		if !ok || len(tpl.Ciphertext) != 16 {
			continue
		}

		orderedWxids := make([]string, 0, len(wxids)+2)
		pushAccountIDCandidate(&orderedWxids, filepath.Base(filepath.Clean(accountPath)))
		for _, w := range wxids {
			pushAccountIDCandidate(&orderedWxids, w)
		}

		for _, wxid := range orderedWxids {
			for _, code := range codes {
				xorKey, aesKey := deriveImageKeyFromCodeWxid(code, wxid)
				keyBytes := []byte(aesKey)
				if !verifyImageAesKeyWeFlow(keyBytes, tpl.Ciphertext) {
					continue
				}
				oldXor := dat2img.V4XorKey
				dat2img.V4XorKey = xorKey
				okStrong := verifyImageAesKeyStrong(keyBytes, tpl.TemplateData)
				if !okStrong {
					dat2img.V4XorKey = oldXor
					continue
				}
				if status != nil {
					status(fmt.Sprintf("命中 code=%d, wxid=%s", code, wxid))
				}
				return aesKey, true
			}
		}
	}

	if len(accountPaths) == 0 {
		xorKey, aesKey := deriveImageKeyFromCodeWxid(codes[0], wxids[0])
		dat2img.V4XorKey = xorKey
		if status != nil {
			status(fmt.Sprintf("模板缺失，回退使用首个 code+wxid（code=%d, wxid=%s）", codes[0], wxids[0]))
		}
		return aesKey, true
	}
	xorKey, aesKey := deriveImageKeyFromCodeWxid(codes[0], wxids[0])
	dat2img.V4XorKey = xorKey
	if status != nil {
		status(fmt.Sprintf("模板验真未命中，回退使用首个 code+wxid（code=%d, wxid=%s）", codes[0], wxids[0]))
	}
	return aesKey, true
}
