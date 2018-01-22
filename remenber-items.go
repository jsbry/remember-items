package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/gin-contrib/multitemplate"
	"github.com/gin-gonic/contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	v2 "google.golang.org/api/oauth2/v2"
)

// AppConf app用config
type AppConf struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURL  string `json:"redirect_url"`
	AuthCode     string `json:"auth_code"`
	CookieSecret string `json:"cookie_secret"`
}

// CallbackRequest コールバックリクエスト
type CallbackRequest struct {
	Code  string `form:"code"`
	State string `form:"state"`
}

var appConf AppConf
var googleConf oauth2.Config

func main() {
	GinRun()
}

// createMyRender テンプレートファイル
func createMyRender() multitemplate.Render {
	r := multitemplate.New()
	r.AddFromFiles("Login", "./templates/login.html")
	r.AddFromFiles("Index", "./templates/index.html")
	r.AddFromFiles("Items", "./templates/items.html")
	return r
}

// GinRun gin実行
func GinRun() {
	SetAppConfig()

	router := gin.Default()
	router.Static("/css", "./css")
	router.Static("/js", "./js")
	router.Static("/img", "./img")
	router.StaticFile("/favicon.ico", "./favicon.ico")

	router.HTMLRender = createMyRender()
	store := sessions.NewCookieStore([]byte(appConf.CookieSecret))
	router.Use(sessions.Sessions("RememberItems", store))

	router.GET("/", Index)
	router.GET("/login", Login)
	router.GET("/items", Items)
	v1 := router.Group("/v1")
	{
		v1.GET("/google/login", v1Login)
		v1.GET("/google/oauthcallback", v1GoogleOAuth)
	}

	router.NoRoute(NoRoute)
	router.Run(":8080")
}

// Login ログイン
func Login(c *gin.Context) {
	c.HTML(200, "Login", gin.H{
		"title": "ログイン｜持ち物管理",
	})
}

// Index トップページ
func Index(c *gin.Context) {
	err := LoginCheck(c)
	if err != nil {
		fmt.Println(err)
		c.Redirect(302, "/login")
		return
	}
	c.HTML(200, "Index", gin.H{
		"title": "持ち物管理",
	})
}

// Items トップページ
func Items(c *gin.Context) {
	err := LoginCheck(c)
	if err != nil {
		fmt.Println(err)
		c.Redirect(302, "/login")
		return
	}
	c.HTML(200, "Items", gin.H{
		"title": "持ち物管理",
	})
}

// v1Login ログイン
func v1Login(c *gin.Context) {
	ClearSession(c)

	url, err := GetGoogleAuthURL()
	if err != nil {
		c.JSON(200, gin.H{
			"code":    500,
			"message": "システムエラーが発生中です",
		})
		return
	}

	c.Redirect(302, url+"&access_type=offline")
}

// v1GoogleOAuth Google認証
func v1GoogleOAuth(c *gin.Context) {
	InitDB()
	defer db.Close()

	err := db.Ping()
	if err != nil {
		c.JSON(200, gin.H{
			"code":    500,
			"message": "データベース接続エラーが発生しました",
		})
		return
	}

	code := c.Query("code")
	state := c.Query("state")

	token, err := GetGoogleCallback(code, state)
	if err != nil {
		c.JSON(200, gin.H{
			"code":    500,
			"message": "認証に失敗しました",
		})
		return
	}

	ID, Email, err := GetGoogleInformaion(token)
	if err != nil {
		c.JSON(200, gin.H{
			"code":    500,
			"message": "認証に失敗しました",
		})
		return
	}

	transaction, err := db.Begin()
	if err != nil {
		c.JSON(200, gin.H{
			"code":    500,
			"message": "データベース接続エラーが発生しました",
		})
		return
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	Expiry := token.Expiry.Format("2006-01-02 15:04:05")
	insertSQL := "INSERT INTO `users`(`google_id`, `google_email`, `google_access_token`, `google_expiry`, `google_refresh_token`, `created`, `modified`) VALUES (?,?,?,?,?,?,?)"
	_, err = transaction.Exec(insertSQL, ID, Email, token.AccessToken, Expiry, token.RefreshToken, now, now)
	if err != nil {
		transaction.Rollback()
		c.JSON(200, gin.H{
			"code":    500,
			"message": "データベース接続エラーが発生しました",
		})
		return
	}
	transaction.Commit()

	session := sessions.Default(c)
	session.Set("accessToken", token.AccessToken)
	session.Save()

	c.Redirect(302, "/")
}

// NoRoute (404)Not Foundページ
func NoRoute(c *gin.Context) {
	c.JSON(404, gin.H{
		"title": "Not Found",
	})
}

// GoogleAccessResponse アクセストークン有効チェック
type GoogleAccessResponse struct {
	Azp           string `json:"azp"`
	Aud           string `json:"aud"`
	Sub           string `json:"sub"`
	Scope         string `json:"scope"`
	Exp           string `json:"exp"`
	ExpiresIn     string `json:"expires_in"`
	Email         string `json:"email"`
	EmailVerified string `json:"email_verified"`
	AccessType    string `json:"access_type"`
}

// LoginCheck ログイン状態
func LoginCheck(c *gin.Context) error {
	session := sessions.Default(c)
	accessToken := session.Get("accessToken")
	if accessToken == nil {
		return errors.New("ログインしてください")
	}

	accessTokenParam := HTTPParam{
		Key:   "access_token",
		Value: accessToken.(string),
	}
	var params []HTTPParam
	params = append(params, accessTokenParam)
	resp, err := RequestGET("https://www.googleapis.com/oauth2/v3/tokeninfo", params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var googleAccessResponse GoogleAccessResponse
	if err := json.NewDecoder(resp.Body).Decode(&googleAccessResponse); err != nil {
		return err
	}
	if googleAccessResponse.Sub == "" {
		return errors.New("アクセストークンの有効期限が切れています")
	}

	InitDB()
	defer db.Close()

	if err = db.Ping(); err != nil {
		return errors.New("データベース接続エラーが発生しました")
	}

	var userID string
	if err := db.QueryRow("SELECT user_id FROM users WHERE google_id = ? AND google_access_token = ? LIMIT 1", googleAccessResponse.Sub, accessToken.(string)).Scan(&userID); err != nil {
		return errors.New("データベース接続エラーが発生しました")
	}
	if userID == "" {
		return errors.New("新規登録してください")
	}

	return nil

	// urlValue := url.Values{
	// 	"client_id":     {appConf.ClientID},
	// 	"client_secret": {appConf.ClientSecret},
	// 	"refresh_token": {"xxxxxxxxxxxxxxxxxxxxxxx"},
	// 	"grant_type":    {"refresh_token"},
	// }

	// resp, err = http.PostForm("https://www.googleapis.com/oauth2/v4/token", urlValue)
	// if err != nil {
	// 	log.Panic("Error when renew token %v", err)
	// }

	// body, err := ioutil.ReadAll(resp.Body)
	// resp.Body.Close()
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// fmt.Printf("\n%s\n", body)
}

// ClearSession セッションクリア
func ClearSession(c *gin.Context) {
	session := sessions.Default(c)
	session.Clear()
	session.Save()
}

// GetGoogleCallback GoogleAuth用callback
func GetGoogleCallback(code, state string) (*oauth2.Token, error) {
	context := context.Background()

	token, err := googleConf.Exchange(context, code)
	if err != nil {
		return nil, err
	}

	if appConf.AuthCode != state {
		return nil, err
	}

	if token.Valid() == false {
		return nil, errors.New("vaild token")
	}

	return token, nil
}

// GetGoogleInformaion GoogleアカウントのIDとEmailを取得
func GetGoogleInformaion(token *oauth2.Token) (string, string, error) {
	context := context.Background()
	service, _ := v2.New(googleConf.Client(context, token))
	tokenInfo, _ := service.Tokeninfo().AccessToken(token.AccessToken).Context(context).Do()

	ID := tokenInfo.UserId
	Email := tokenInfo.Email

	return ID, Email, nil
}

// HTTPParam リクエスト用パラメータ
type HTTPParam struct {
	Key   string
	Value string
}

// RequestGET GETリクエスト
func RequestGET(target string, params []HTTPParam) (*http.Response, error) {
	values := url.Values{}
	for _, param := range params {
		values.Add(param.Key, param.Value)
	}
	resp, err := http.Get(target + "?" + values.Encode())
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// SetAppConfig Google用Config
func SetAppConfig() error {
	jsonString, err := ioutil.ReadFile("appConf.json")
	if err != nil {
		return err
	}
	err = json.Unmarshal(jsonString, &appConf)
	if err != nil {
		return err
	}

	googleConf = oauth2.Config{
		ClientID:     appConf.ClientID,
		ClientSecret: appConf.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{"openid", "email"},
		RedirectURL:  appConf.RedirectURL,
	}
	return nil
}

// GetGoogleAuthURL GoogleAuthCodeURLを取得
func GetGoogleAuthURL() (string, error) {
	url := googleConf.AuthCodeURL(appConf.AuthCode)
	return url, nil
}

// DbConfig データベース接続用struct
type DbConfig struct {
	Dsn      string `json:"dsn"`
	Username string `json:"username"`
	Password string `json:"password"`
	Server   string `json:"server"`
	Database string `json:"database"`
	Charset  string `json:"charset"`
}

var dbConf DbConfig
var db *sql.DB

// InitDB データベース接続
func InitDB() error {
	jsonString, err := ioutil.ReadFile("dbConf.json")
	if err != nil {
		return err
	}
	err = json.Unmarshal(jsonString, &dbConf)
	if err != nil {
		return err
	}

	connect := fmt.Sprintf(dbConf.Dsn, dbConf.Username, dbConf.Password, dbConf.Server, dbConf.Database, dbConf.Charset)
	db, err = sql.Open("mysql", connect)

	if err != nil {
		return err
	}

	return nil
}
