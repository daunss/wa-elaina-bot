package util
import ("io"; "net/http"; "strconv"; "strings"; "fmt")

type HTTP struct{ Client *http.Client }

func HeadInfo(c *http.Client, u string) (int64, string, error) {
  req,_ := http.NewRequest(http.MethodHead, u, nil)
  req.Header.Set("User-Agent","wa-elaina-bot/1.0")
  resp, err := c.Do(req); if err!=nil { return 0,"",err }
  defer resp.Body.Close()
  cl := strings.TrimSpace(resp.Header.Get("Content-Length"))
  var size int64; if cl!="" { if n, e := strconv.ParseInt(cl,10,64); e==nil { size=n } }
  return size, strings.TrimSpace(resp.Header.Get("Content-Type")), nil
}

func DownloadBytes(c *http.Client, u string, max int64) ([]byte, string, error) {
  req,_ := http.NewRequest(http.MethodGet, u, nil)
  req.Header.Set("User-Agent","wa-elaina-bot/1.0")
  resp, err := c.Do(req); if err!=nil { return nil,"",err }
  defer resp.Body.Close()
  ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
  if max>0 && resp.ContentLength>0 && resp.ContentLength>max { return nil, ct, fmt.Errorf("file too large: %d > %d", resp.ContentLength, max) }
  var r io.Reader = resp.Body; if max>0 { r = io.LimitReader(resp.Body, max+1) }
  data, err := io.ReadAll(r); if err!=nil { return nil, ct, err }
  if max>0 && int64(len(data))>max { return nil, ct, fmt.Errorf("file too large after read: %d > %d", len(data), max) }
  if resp.StatusCode >= 300 { return nil, ct, fmt.Errorf("http %s", resp.Status) }
  return data, ct, nil
}
