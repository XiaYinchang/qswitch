package antigravity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"qswitch/internal/adapter"
	"qswitch/internal/busy"
)

const cloudEndpointCacheName = "antigravity-cloud-endpoint.json"

type cloudEndpointObservation struct {
	Endpoint   string `json:"endpoint"`
	ObservedAt int64  `json:"observed_at"`
	Source     string `json:"source"`
}

// CloudCodeEndpoint follows native runtime arguments or a managed CLI's private
// launch record. Only when no native process is running may a previous verified
// local observation be used. Uncertain live state never falls back to the cache.
func CloudCodeEndpoint(home, dataDir string, list func() ([]adapter.Proc, error)) (string, error) {
	if list == nil {
		list = (busy.PS{}).List
	}
	processes, err := list()
	if err != nil {
		return "", errors.New("antigravity: 无法确认原生服务的额度后端")
	}
	var native []adapter.Proc
	for _, process := range processes {
		if process.PID > 0 && nativeRPCProcess(process.Command) {
			native = append(native, process)
		}
	}
	verified := make(map[int]bool)
	managed := make(map[int]string)
	if len(native) > 0 {
		endpoints, err := DiscoverManaged(home, dataDir, func() ([]adapter.Proc, error) { return processes, nil })
		if err != nil {
			return "", errors.New("antigravity: 无法验证原生服务的本机身份")
		}
		for _, endpoint := range endpoints {
			verified[endpoint.PID] = true
			if endpoint.CloudCodeEndpoint != "" {
				if prior := managed[endpoint.PID]; prior != "" && prior != endpoint.CloudCodeEndpoint {
					return "", errors.New("antigravity: 受管 CLI 的额度后端记录冲突")
				}
				managed[endpoint.PID] = endpoint.CloudCodeEndpoint
			}
		}
	}
	var endpoint string
	for _, process := range native {
		if !verified[process.PID] {
			return "", errors.New("antigravity: 原生服务缺少可验证的本机记录")
		}
		current := managed[process.PID]
		if current == "" {
			current, err = processCloudEndpoint(process.Command)
			if err != nil {
				return "", err
			}
		}
		if endpoint != "" && endpoint != current {
			return "", errors.New("antigravity: 正在运行的原生服务使用不同额度后端")
		}
		endpoint = current
	}
	if dataDir == "" {
		return "", errors.New("antigravity: 未配置本机额度后端记录目录")
	}
	path := filepath.Join(dataDir, cloudEndpointCacheName)
	if endpoint == "" {
		raw, err := readDaemonRecord(path)
		if err != nil {
			return "", errors.New("antigravity: 没有已确认的原生额度后端，请先启动原生客户端")
		}
		var observed cloudEndpointObservation
		if json.Unmarshal(raw, &observed) != nil || observed.Source != "native_process" || observed.ObservedAt <= 0 || observed.ObservedAt > time.Now().Add(5*time.Second).Unix() {
			return "", errors.New("antigravity: 本机额度后端记录无效")
		}
		return allowedCloudEndpoint(observed.Endpoint)
	}
	if err := writeCloudEndpointObservation(path, cloudEndpointObservation{Endpoint: endpoint, ObservedAt: time.Now().Unix(), Source: "native_process"}); err != nil {
		return "", errors.New("antigravity: 无法保存已确认的原生额度后端")
	}
	return endpoint, nil
}

func allowedCloudEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSuffix(endpoint, "/")
	switch endpoint {
	case "https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com":
		return endpoint, nil
	default:
		return "", errors.New("antigravity: 原生额度后端不在受支持的官方地址中")
	}
}

func processCloudEndpoint(command string) (string, error) {
	args, err := cloudEndpointArgs(command)
	if err != nil {
		return "", err
	}
	var endpoint string
	found := false
	for i := 1; i < len(args); i++ {
		var value string
		switch {
		case args[i] == "--cloud_code_endpoint":
			i++
			if i >= len(args) {
				return "", errors.New("antigravity: 原生服务的额度后端参数缺少地址")
			}
			value = args[i]
		case strings.HasPrefix(args[i], "--cloud_code_endpoint="):
			value = strings.TrimPrefix(args[i], "--cloud_code_endpoint=")
		default:
			continue
		}
		if found {
			return "", errors.New("antigravity: 原生服务的额度后端参数重复")
		}
		found = true
		endpoint, err = allowedCloudEndpoint(value)
		if err != nil {
			return "", err
		}
	}
	if !found {
		return "", errors.New("antigravity: 原生服务未明确声明额度后端")
	}
	return endpoint, nil
}

// Preserve Windows path backslashes; quotes delimit process arguments rather
// than shell syntax. No shell expansion or command execution is performed.
func cloudEndpointArgs(command string) ([]string, error) {
	var args []string
	var part strings.Builder
	var quote rune
	started := false
	for _, char := range command {
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				part.WriteRune(char)
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			started = true
			continue
		}
		if unicode.IsSpace(char) {
			if started {
				args = append(args, part.String())
				part.Reset()
				started = false
			}
			continue
		}
		part.WriteRune(char)
		started = true
	}
	if quote != 0 {
		return nil, errors.New("antigravity: 无法明确解析原生服务参数")
	}
	if started {
		args = append(args, part.String())
	}
	return args, nil
}

func writeCloudEndpointObservation(path string, observed cloudEndpointObservation) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temporary := path + "." + hex.EncodeToString(random[:]) + ".pending"
	file, err := createPrivateMetadataFile(temporary)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	err = json.NewEncoder(file).Encode(observed)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temporary, path)
}
