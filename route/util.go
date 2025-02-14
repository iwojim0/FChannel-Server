package route

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html/template"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FChannel0/FChannel-Server/util"

	"github.com/FChannel0/FChannel-Server/activitypub"
	"github.com/FChannel0/FChannel-Server/config"
	"github.com/FChannel0/FChannel-Server/db"
	"github.com/FChannel0/FChannel-Server/post"
	"github.com/FChannel0/FChannel-Server/webfinger"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/template/html"
	country "github.com/mikekonan/go-countries"
)

func GetThemeCookie(c *fiber.Ctx) string {
	cookie := c.Cookies("theme")
	if cookie != "" {
		cookies := strings.SplitN(cookie, "=", 2)
		return cookies[0]
	}

	return "default"
}

func WantToServeCatalog(actorName string) (activitypub.Collection, bool, error) {
	var collection activitypub.Collection
	serve := false

	actor, err := activitypub.GetActorByNameFromDB(actorName)
	if err != nil {
		return collection, false, util.MakeError(err, "WantToServeCatalog")
	}

	if actor.Id != "" {
		collection, err = actor.GetCatalogCollection()
		if err != nil {
			return collection, false, util.MakeError(err, "WantToServeCatalog")
		}

		collection.Actor = actor
		return collection, true, nil
	}

	return collection, serve, nil
}

func WantToServeArchive(actorName string) (activitypub.Collection, bool, error) {
	var collection activitypub.Collection
	serve := false

	actor, err := activitypub.GetActorByNameFromDB(actorName)
	if err != nil {
		return collection, false, util.MakeError(err, "WantToServeArchive")
	}

	if actor.Id != "" {
		collection, err = actor.GetCollectionType("Archive")
		if err != nil {
			return collection, false, util.MakeError(err, "WantToServeArchive")
		}

		collection.Actor = actor
		return collection, true, nil
	}

	return collection, serve, nil
}

func GetActorPost(ctx *fiber.Ctx, path string) error {
	obj := activitypub.ObjectBase{Id: config.Domain + "" + path}
	collection, err := obj.GetCollectionFromPath()

	if err != nil {
		return util.MakeError(err, "GetActorPost")
	}

	if len(collection.OrderedItems) > 0 {
		enc, err := json.MarshalIndent(collection, "", "\t")
		if err != nil {
			return util.MakeError(err, "GetActorPost")
		}

		ctx.Response().Header.Set("Content-Type", "application/ld+json; profile=\"https://www.w3.org/ns/activitystreams\"")
		_, err = ctx.Write(enc)
		return util.MakeError(err, "GetActorPost")
	}

	return nil
}

func ParseOutboxRequest(ctx *fiber.Ctx, actor activitypub.Actor) error {
	contentType := util.GetContentType(ctx.Get("content-type"))

	if contentType == "multipart/form-data" || contentType == "application/x-www-form-urlencoded" {
		hasCaptcha, err := util.BoardHasAuthType(actor.Name, "captcha")
		if err != nil {
			return util.MakeError(err, "ParseOutboxRequest")
		}

		valid, err := post.CheckCaptcha(ctx.FormValue("captcha"))
		if err == nil && hasCaptcha && valid {
			header, _ := ctx.FormFile("file")
			if header != nil {
				f, _ := header.Open()
				defer f.Close()
				if header.Size > (12 << 20) {
					ctx.Response().Header.SetStatusCode(403)
					_, err := ctx.Write([]byte("12MB max file size"))
					return util.MakeError(err, "ParseOutboxRequest")
				} else if isBanned, err := post.IsMediaBanned(f); err == nil && isBanned {
					config.Log.Println("media banned")
					ctx.Response().Header.SetStatusCode(403)
					_, err := ctx.Write([]byte(""))
					return util.MakeError(err, "ParseOutboxRequest")
				} else if err != nil {
					return util.MakeError(err, "ParseOutboxRequest")
				}

				contentType, _ := util.GetFileContentType(f)
				if actor.Name == "f" && len(util.EscapeString(ctx.FormValue("inReplyTo"))) == 0 && contentType != "application/x-shockwave-flash" {
					ctx.Response().Header.SetStatusCode(403)
					_, err := ctx.Write([]byte("file type not supported"))
					return util.MakeError(err, "ParseOutboxRequest")
				}

				if !post.SupportedMIMEType(contentType) {
					ctx.Response().Header.SetStatusCode(403)
					_, err := ctx.Write([]byte("file type not supported"))
					return util.MakeError(err, "ParseOutboxRequest")
				}
			}

			var nObj = activitypub.CreateObject("Note")
			nObj, err := post.ObjectFromForm(ctx, nObj)
			if err != nil {
				return util.MakeError(err, "ParseOutboxRequest")
			}

			op := len(nObj.InReplyTo) - 1
			if op >= 0 {
				if nObj.InReplyTo[op].Id == "" {
					if actor.Name == "overboard" {
						return ctx.SendStatus(400)
					}
				}
			}

			if actor.Name == "int" || actor.Name == "bint" {
				nObj.Alias = "cc:" + util.GetCC(ctx.Get("PosterIP"))
			}

			if actor.Name == "bint" {
				//TODO: better way to pass IP to
				if ctx.Get("PosterIP") == "172.16.0.1" || util.IsTorExit(ctx.Get("PosterIP")) {
					nObj.Alias = nObj.Alias + "id:HiddenID"
				} else {
					input := []byte(ctx.Get("PosterIP"))
					hasher := sha256.New()
					hasher.Write(input)
					sha := base64.URLEncoding.EncodeToString(hasher.Sum(nil))

					uniqID := string(sha)

					nObj.Alias = nObj.Alias + "id:" + uniqID
				}
			}

			nObj.Actor = config.Domain + "/" + actor.Name

			if locked, _ := nObj.InReplyTo[0].IsLocked(); locked {
				ctx.Response().Header.SetStatusCode(403)
				_, err := ctx.Write([]byte("thread is locked"))
				return util.MakeError(err, "ParseOutboxRequest")
			}

			nObj, err = nObj.Write()
			if err != nil {
				return util.MakeError(err, "ParseOutboxRequest")
			}

			if len(nObj.To) == 0 {
				if err := actor.ArchivePosts(); err != nil {
					return util.MakeError(err, "ParseOutboxRequest")
				}
			}

			go func(nObj activitypub.ObjectBase) {
				activity, err := nObj.CreateActivity("Create")
				if err != nil {
					config.Log.Printf("ParseOutboxRequest Create Activity: %s", err)
				}

				activity, err = activity.AddFollowersTo()
				if err != nil {
					config.Log.Printf("ParseOutboxRequest Add FollowersTo: %s", err)
				}

				if err := activity.MakeRequestInbox(); err != nil {
					config.Log.Printf("ParseOutboxRequest MakeRequestInbox: %s", err)
				}
			}(nObj)

			go func(obj activitypub.ObjectBase) {
				err := obj.SendEmailNotify()

				if err != nil {
					config.Log.Println(err)
				}
			}(nObj)

			var id string
			//op := len(nObj.InReplyTo) - 1
			if op >= 0 {
				if nObj.InReplyTo[op].Id == "" {
					if actor.Name == "overboard" {
						return ctx.SendStatus(400)
					}
					id = nObj.Id
				} else {
					id = nObj.InReplyTo[0].Id + "|" + nObj.Id
				}
			}

			if len(ctx.Get("PosterIP")) > 1 || len(ctx.Get("pwd")) > 0 {
				query := `INSERT INTO "identify" (id, ip, password) VALUES ($1, $2, crypt($3, gen_salt('bf')))`
				_, err = config.DB.Exec(query, nObj.Id, ctx.Get("PosterIP"), ctx.FormValue("pwd"))
				if err != nil {
					return util.MakeError(err, "ParseOutboxRequest")
				}
			}

			ctx.Response().Header.Set("Status", "200")
			_, err = ctx.Write([]byte(id))
			return util.MakeError(err, "ParseOutboxRequest")
		} else {
			return Send403(ctx, "Incorrect captcha")
		}
	} else { // json request
		activity, err := activitypub.GetActivityFromJson(ctx)
		if err != nil {
			return util.MakeError(err, "ParseOutboxRequest")
		}

		if res, _ := activity.IsLocal(); res {
			if res := activity.Actor.VerifyHeaderSignature(ctx); err == nil && !res {
				ctx.Response().Header.Set("Status", "403")
				_, err = ctx.Write([]byte(""))
				return util.MakeError(err, "ParseOutboxRequest")
			}

			switch activity.Type {
			case "Create":
				ctx.Response().Header.Set("Status", "403")
				_, err = ctx.Write([]byte(""))
				break

			case "Follow":
				validActor := (activity.Object.Actor != "")
				validLocalActor := (activity.Actor.Id == actor.Id)

				var rActivity activitypub.Activity

				if validActor && validLocalActor {
					rActivity = activity.AcceptFollow()
					rActivity, err = rActivity.SetActorFollowing()

					if err != nil {
						return util.MakeError(err, "ParseOutboxRequest")
					}

					if err := activity.MakeRequestInbox(); err != nil {
						return util.MakeError(err, "ParseOutboxRequest")
					}
				}

				actor, _ := activitypub.GetActorFromDB(config.Domain)
				webfinger.FollowingBoards, err = actor.GetFollowing()

				if err != nil {
					return util.MakeError(err, "ParseOutboxRequest")
				}

				webfinger.Boards, err = webfinger.GetBoardCollection()

				if err != nil {
					return util.MakeError(err, "ParseOutboxRequest")
				}
				break

			case "Delete":
				config.Log.Println("This is a delete")
				ctx.Response().Header.Set("Status", "403")
				_, err = ctx.Write([]byte("could not process activity"))
				break

			case "Note":
				ctx.Response().Header.Set("Status", "403")
				_, err = ctx.Write([]byte("could not process activity"))
				break

			case "New":
				name := activity.Object.Alias
				prefname := activity.Object.Name
				summary := activity.Object.Summary
				restricted := activity.Object.Sensitive

				actor, err := db.CreateNewBoard(*activitypub.CreateNewActor(name, prefname, summary, config.AuthReq, restricted))
				if err != nil {
					return util.MakeError(err, "ParseOutboxRequest")
				}

				if actor.Id != "" {
					var board []activitypub.ObjectBase
					var item activitypub.ObjectBase
					var removed bool = false

					item.Id = actor.Id
					for _, e := range webfinger.FollowingBoards {
						if e.Id != item.Id {
							board = append(board, e)
						} else {
							removed = true
						}
					}

					if !removed {
						board = append(board, item)
					}

					webfinger.FollowingBoards = board
					webfinger.Boards, err = webfinger.GetBoardCollection()
					return util.MakeError(err, "ParseOutboxRequest")
				}

				ctx.Response().Header.Set("Status", "403")
				_, err = ctx.Write([]byte(""))
				break

			default:
				ctx.Response().Header.Set("status", "403")
				_, err = ctx.Write([]byte("could not process activity"))
			}
		} else if err != nil {
			return util.MakeError(err, "ParseOutboxRequest")
		} else {
			config.Log.Println("is NOT activity")
			ctx.Response().Header.Set("Status", "403")
			_, err = ctx.Write([]byte("could not process activity"))
			return util.MakeError(err, "ParseOutboxRequest")
		}
	}

	return nil
}

func TemplateFunctions(engine *html.Engine) {
	engine.AddFunc("mod", func(i, j int) bool {
		return i%j == 0
	})

	engine.AddFunc("sub", func(i, j int) int {
		return i - j
	})

	engine.AddFunc("add", func(i, j int) int {
		return i + j
	})

	engine.AddFunc("unixtoreadable", func(u int) string {
		return time.Unix(int64(u), 0).Format("Jan 02, 2006")
	})

	engine.AddFunc("timeToDateLong", func(t time.Time) string {
		day := t.Day()
		suffix := "th"
		switch day {
		case 1, 21, 31:
			suffix = "st"
		case 2, 22:
			suffix = "nd"
		case 3, 23:
			suffix = "rd"
		}
		return t.Format("January 2" + suffix + ", 2006 MST")
	})

	engine.AddFunc("timeToDateTimeLong", func(t time.Time) string {
		day := t.Day()
		suffix := "th"
		switch day {
		case 1, 21, 31:
			suffix = "st"
		case 2, 22:
			suffix = "nd"
		case 3, 23:
			suffix = "rd"
		}
		return t.Format("January 2" + suffix + ", 2006 at 15:04 UTC")
	})

	engine.AddFunc("timeToReadableLong", func(t time.Time) string {
		return t.Format("01/02/06(Mon)15:04:05")
	})

	engine.AddFunc("timeToUnix", func(t time.Time) string {
		return fmt.Sprint(t.Unix())
	})

	engine.AddFunc("proxy", util.MediaProxy)

	// previously short
	engine.AddFunc("shortURL", util.ShortURL)

	engine.AddFunc("parseAttachment", post.ParseAttachment)

	engine.AddFunc("parseContent", post.ParseContent)

	engine.AddFunc("formatContent", post.FormatContent)

	engine.AddFunc("shortImg", util.ShortImg)

	engine.AddFunc("convertSize", util.ConvertSize)

	engine.AddFunc("isOnion", util.IsOnion)

	engine.AddFunc("parseReplyLink", func(actorId string, op string, id string, content string) template.HTML {
		actor, _ := activitypub.FingerActor(actorId)
		title := strings.ReplaceAll(post.ParseLinkTitle(actor.Id+"/", op, content), `/\&lt;`, ">")
		link := "<a href=\"/" + actor.Name + "/" + util.ShortURL(actor.Outbox, op) + "#" + util.ShortURL(actor.Outbox, id) + "\" title=\"" + title + "\" class=\"replyLink\">&gt;&gt;" + util.ShortURL(actor.Outbox, id) + "</a>"
		return template.HTML(link)
	})

	engine.AddFunc("shortExcerpt", func(post activitypub.ObjectBase) template.HTML {
		var returnString string

		if post.Name != "" {
			returnString = post.Name + "| " + post.Content
		} else {
			returnString = post.Content
		}

		re := regexp.MustCompile(`(^(.|\r\n|\n){100})`)

		match := re.FindStringSubmatch(returnString)

		if len(match) > 0 {
			returnString = match[0] + "..."
		}

		returnString = strings.ReplaceAll(returnString, "<", "&lt;")
		returnString = strings.ReplaceAll(returnString, ">", "&gt;")

		re = regexp.MustCompile(`(^.+\|)`)

		match = re.FindStringSubmatch(returnString)

		if len(match) > 0 {
			returnString = strings.Replace(returnString, match[0], "<b>"+match[0]+"</b>", 1)
			returnString = strings.Replace(returnString, "|", ":", 1)
		}

		return template.HTML(returnString)
	})

	engine.AddFunc("parseLinkTitle", func(board string, op string, content string) string {
		nContent := post.ParseLinkTitle(board, op, content)
		nContent = strings.ReplaceAll(nContent, `/\&lt;`, ">")

		return nContent
	})

	engine.AddFunc("parseLink", func(board activitypub.Actor, link string) string {
		var obj = activitypub.ObjectBase{
			Id: link,
		}

		var OP string
		if OP, _ = obj.GetOP(); OP == obj.Id {
			return board.Name + "/" + util.ShortURL(board.Outbox, obj.Id)
		}

		return board.Name + "/" + util.ShortURL(board.Outbox, OP) + "#" + util.ShortURL(board.Outbox, link)
	})

	engine.AddFunc("showArchive", func(actor activitypub.Actor) bool {
		col, err := actor.GetCollectionTypeLimit("Archive", 1)

		if err != nil || len(col.OrderedItems) == 0 {
			return false
		}

		return true
	})

	engine.AddFunc("parseIDandFlag", func(input string) template.HTML {
		var html string
		re := regexp.MustCompile(`id:\S{8}`)
		id := re.FindString(input)

		re = regexp.MustCompile(`cc:\S{2}`)
		cc := re.FindString(input)

		if id != "" {
			var r, g, b int
			var txtcol, bgcol string
			//var shadcol string
			id = strings.TrimPrefix(id, "id:")
			if id == "HiddenID" {
				bgcol = "rgb(255, 255, 255)"
				txtcol = "#000"
			} else {
				h := md5.New()
				h.Write([]byte(id))
				var seed uint64 = binary.BigEndian.Uint64(h.Sum(nil))
				rand.Seed(int64(seed))
				r = rand.Intn(256)
				g = rand.Intn(256)
				b = rand.Intn(256)
				bgcol = "rgb(" + strconv.Itoa(r) + ", " + strconv.Itoa(g) + ", " + strconv.Itoa(b) + ")"
				var l float64 = ((0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 255)
				if l > 0.5 {
					txtcol = "#000"
				} else {
					txtcol = "#FFF"
				}
			}
			html = " <span class=\"posteruid id_" + id + "\">(ID: <span class=\"id\" style=\"background-color: " + bgcol + "; color: " + txtcol + ";\">" + id + "</span>)</span>"
		}
		if cc != "" {
			var countryname string
			cc = strings.TrimPrefix(cc, "cc:")
			//TODO: remove external library for country
			switch cc {
			case "xp":
				countryname = "Tor/Proxy"
			default:
				if posterCountry, ok := country.ByAlpha2CodeStr(cc); ok {
					countryname = posterCountry.Name().String()
				} else {
					countryname = "Unknown/Hidden"
				}
			}
			html = html + " <span title=\"" + countryname + "\" class=\"flag flag-" + cc + "\"></span>"
		}
		return template.HTML(html)
	})

	engine.AddFunc("parseEmail", func(input string) template.HTML {
		var html string
		if len(input) > 1 {
			email := regexp.MustCompile("email:.+@.+\\..+")
			if email.MatchString(input) {
				addr := strings.TrimPrefix(input, "email:")
				html += "<a href='mailto:" + addr + "' class='userEmail'>"
			}
		}
		return template.HTML(html)
	})

	engine.AddFunc("timeUntil", func(to time.Time, from ...time.Time) string {
		var duration time.Duration
		if len(from) > 0 {
			duration = to.Sub(from[0].UTC())
		} else {
			duration = to.Sub(time.Now().UTC())
		}
		years := int(duration.Hours() / 24 / 365)
		months := int(duration.Hours()/24/30) % 12
		days := int(duration.Hours()/24) % 30
		hours := int(duration.Hours()) % 24
		minutes := int(duration.Minutes()) % 60
		seconds := int(duration.Seconds()) % 60

		var timeStrings []string
		if years > 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d years", years))
		} else if years == 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d year", years))
		}
		if months > 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d months", months))
		} else if months == 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d month", months))
		}
		if days > 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d days", days))
		} else if days == 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d day", days))
		}
		if hours > 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d hours", hours))
		} else if hours == 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d hour", hours))
		}
		if minutes > 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d minutes", minutes))
		} else if minutes == 1 {
			timeStrings = append(timeStrings, fmt.Sprintf("%d minute", minutes))
		}
		if years == 0 && months == 0 && days == 0 && minutes == 0 {
			if seconds == 1 {
				timeStrings = append(timeStrings, fmt.Sprintf("%d second", seconds))
			} else if seconds > 1 {
				timeStrings = append(timeStrings, fmt.Sprintf("%d seconds", seconds))
			}
		}

		if len(timeStrings) == 0 {
			return "0 seconds"
		}

		if len(timeStrings) == 1 {
			return timeStrings[0]
		}

		last := timeStrings[len(timeStrings)-1]
		timeStrings = timeStrings[:len(timeStrings)-1]
		return strings.Join(timeStrings, ", ") + " and " + last
	})

	engine.AddFunc("maxFileSize", func() string {
		return util.ConvertSize(int64(config.MaxAttachmentSize))
	})
}

func StatusTemplate(num int) func(ctx *fiber.Ctx, msg ...string) error {
	n := fmt.Sprint(num)
	return func(ctx *fiber.Ctx, msg ...string) error {
		var m string
		if msg != nil {
			m = msg[0]
		}

		var data PageData
		var errorData errorData

		data.Boards = webfinger.Boards
		data.Themes = &config.Themes
		data.ThemeCookie = GetThemeCookie(ctx)
		data.Referer = ctx.Get("referer")

		errorData.Message = m

		return ctx.Status(num).Render(n, fiber.Map{
			"page":  data,
			"error": errorData,
		}, "layouts/main")
	}
}

func GenericError(ctx *fiber.Ctx, msg ...string) error {

	var m string
	if msg != nil {
		m = msg[0]
	}

	var data PageData
	var errorData errorData

	data.Boards = webfinger.Boards
	data.Themes = &config.Themes
	data.ThemeCookie = GetThemeCookie(ctx)
	data.Referer = ctx.Get("referer")

	errorData.Message = m

	return ctx.Status(400).Render("gerror", fiber.Map{
		"page":  data,
		"error": errorData,
	}, "layouts/main")
}

func Send500(ctx *fiber.Ctx, err error, msg ...string) error {

	var m string
	if msg != nil {
		m = msg[0]
	}

	var data PageData
	var errorData errorData

	data.Boards = webfinger.Boards
	data.Themes = &config.Themes
	data.ThemeCookie = GetThemeCookie(ctx)
	data.Referer = ctx.Get("referer")

	errorData.Message = m
	errorData.Error = err

	// The results of this call do not matter to us
	ctx.Status(500).Render("500", fiber.Map{
		"page":  data,
		"error": errorData,
	}, "layouts/main")

	return err
}

var Send400 = StatusTemplate(400)
var Send403 = StatusTemplate(403)
var Send404 = StatusTemplate(404)
