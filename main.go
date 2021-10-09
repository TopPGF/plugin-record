package record

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/Monibuca/engine/v3"
	. "github.com/Monibuca/utils/v3"
)

var config struct {
	Path       string
	Append     bool
	AutoRecord bool
}
var recordings sync.Map

type FlvFileInfo struct {
	Path     string
	Size     int64
	Duration uint32
}

type FileWr interface {
	io.Reader
	io.Writer
	io.Seeker
	io.Closer
}

var ExtraConfig struct {
	CreateFileFn     func(filename string) (FileWr, error)
	AutoRecordFilter func(stream string) bool
}

func init() {
	InstallPlugin(&PluginConfig{
		Name:   "Record",
		Config: &config,
		Run:    run,
		HotConfig: map[string]func(interface{}){
			"AutoRecord": func(v interface{}) {
				config.AutoRecord = v.(bool)
			},
		},
	})
}
func run() {

	go AddHook(HOOK_PUBLISH, onPublish)
	recordTicket()
	os.MkdirAll(config.Path, 0755)
	http.HandleFunc("/vod/", VodHandler)
	http.HandleFunc("/api/record/flv/list", func(w http.ResponseWriter, r *http.Request) {
		CORS(w, r)
		if files, err := tree(config.Path, 0); err == nil {
			var bytes []byte
			if bytes, err = json.Marshal(files); err == nil {
				w.Write(bytes)
			} else {
				w.Write([]byte("{\"err\":\"" + err.Error() + "\"}"))
			}
		} else {
			w.Write([]byte("{\"err\":\"" + err.Error() + "\"}"))
		}
	})
	http.HandleFunc("/api/record/flv", func(w http.ResponseWriter, r *http.Request) {
		CORS(w, r)
		if streamPath := r.URL.Query().Get("streamPath"); streamPath != "" {
			if err := SaveFlv(streamPath, r.URL.Query().Get("append") == "true"); err != nil {
				w.Write([]byte(err.Error()))
			} else {
				w.Write([]byte("success"))
			}
		} else {
			w.Write([]byte("no streamPath"))
		}
	})

	http.HandleFunc("/api/record/flv/stop", func(w http.ResponseWriter, r *http.Request) {
		CORS(w, r)
		if streamPath := r.URL.Query().Get("streamPath"); streamPath != "" {
			hasStream := false
			filePath := filepath.Join(config.Path, streamPath)
			recordings.Range(func(key, stream interface{}) bool {
				streamPath := fmt.Sprintf("%v", key)
				if strings.Contains(streamPath, filePath) {
					hasStream = true
					output := stream.(*Subscriber)
					output.Close()
					recordings.Delete(key)
				}
				return true
			})
			if hasStream {
				w.Write([]byte("success"))
			} else {
				w.Write([]byte("no query stream"))
			}

		} else {
			w.Write([]byte("no such stream"))
		}
	})
	http.HandleFunc("/api/record/flv/play", func(w http.ResponseWriter, r *http.Request) {
		CORS(w, r)
		if streamPath := r.URL.Query().Get("streamPath"); streamPath != "" {
			if err := PublishFlvFile(streamPath); err != nil {
				w.Write([]byte(err.Error()))
			} else {
				w.Write([]byte("success"))
			}
		} else {
			w.Write([]byte("no streamPath"))
		}
	})
	http.HandleFunc("/api/record/flv/delete", func(w http.ResponseWriter, r *http.Request) {
		CORS(w, r)
		if streamPath := r.URL.Query().Get("streamPath"); streamPath != "" {
			filePath := filepath.Join(config.Path, streamPath+".flv")
			if Exist(filePath) {
				if err := os.Remove(filePath); err != nil {
					w.Write([]byte(err.Error()))
				} else {
					w.Write([]byte("success"))
				}
			} else {
				w.Write([]byte("no such file"))
			}
		} else {
			w.Write([]byte("no streamPath"))
		}
	})
}

func onPublish(p *Stream) {
	if config.AutoRecord || (ExtraConfig.AutoRecordFilter != nil && ExtraConfig.AutoRecordFilter(p.StreamPath)) {
		SaveFlv(p.StreamPath, config.Append)
	}
}

//recordTicket 定时拉起新流写入新文件
func recordTicket() {
	ticker := time.NewTicker(time.Second)
	go func() {
		for _ = range ticker.C {
			timeObj := time.Now()
			//每小时保存
			if timeObj.Format("0405") == "0000" {
				//if timeObj.Format("05") == "00" {
				recordings.Range(func(key, stream interface{}) bool {
					streamPath := fmt.Sprintf("%v", key)
					fmt.Println("recordTicket do " + streamPath)
					subscriber := stream.(*Subscriber)
					if subscriber == nil {
						recordings.Delete(key)
						fmt.Println("SaveFlv " + streamPath + " subscriber is nil")
						return true
					}
					if subscriber.Context == nil {
						recordings.Delete(key)
						fmt.Println("SaveFlv " + streamPath + " subscriber Context is nil")
						return true
					}
					if subscriber.Context.Err() != nil {
						subscriber.Close()
						recordings.Delete(key)
						fmt.Println("SaveFlv " + streamPath + " subscriber is close")
						//return true
					}

					streamPathSplit := strings.Split(streamPath, "/")
					if len(streamPathSplit) != 4 {
						return true
					}
					streamPath = streamPathSplit[1] + "/" + streamPathSplit[2]
					go func() {
						if err := SaveFlv(streamPath, false); err != nil {
							fmt.Println("SaveFlv " + streamPath + " error," + err.Error())
						} else {
							output := stream.(*Subscriber)
							output.Close()
							recordings.Delete(key)
						}
					}()
					return true
				})
			}
		}
	}()
}

func tree(dstPath string, level int) (files []*FlvFileInfo, err error) {
	var dstF *os.File
	dstF, err = os.Open(dstPath)
	if err != nil {
		return
	}
	defer dstF.Close()
	fileInfo, err := dstF.Stat()
	if err != nil {
		return
	}
	if !fileInfo.IsDir() { //如果dstF是文件
		if path.Ext(fileInfo.Name()) == ".flv" {
			p := strings.TrimPrefix(dstPath, config.Path)
			p = strings.ReplaceAll(p, "\\", "/")
			files = append(files, &FlvFileInfo{
				Path:     strings.TrimPrefix(p, "/"),
				Size:     fileInfo.Size(),
				Duration: getDuration(dstF),
			})
		}
		return
	} else { //如果dstF是文件夹
		var dir []os.FileInfo
		dir, err = dstF.Readdir(0) //获取文件夹下各个文件或文件夹的fileInfo
		if err != nil {
			return
		}
		for _, fileInfo = range dir {
			var _files []*FlvFileInfo
			_files, err = tree(filepath.Join(dstPath, fileInfo.Name()), level+1)
			if err != nil {
				return
			}
			files = append(files, _files...)
		}
		return
	}

}
