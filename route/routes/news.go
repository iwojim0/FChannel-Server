package routes

import (
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/FChannel0/FChannel-Server/activitypub"
	"github.com/FChannel0/FChannel-Server/config"
	"github.com/FChannel0/FChannel-Server/db"
	"github.com/FChannel0/FChannel-Server/route"
	"github.com/FChannel0/FChannel-Server/util"
	"github.com/FChannel0/FChannel-Server/webfinger"
	"github.com/gofiber/fiber/v2"
	"github.com/gorilla/feeds"
)

func NewsGet(ctx *fiber.Ctx) error {
	timestamp := ctx.Path()[6:]
	ts, err := strconv.Atoi(timestamp)

	if err != nil {
		return ctx.Status(404).Render("404", fiber.Map{})
	}

	actor, err := activitypub.GetActorFromDB(config.Domain)

	if err != nil {
		return util.MakeError(err, "NewsGet")
	}

	var data route.PageData
	data.PreferredUsername = actor.PreferredUsername
	data.Boards = webfinger.Boards
	data.Board.Name = ""
	data.Key = config.Key
	data.Board.Domain = config.Domain
	data.Board.ModCred, _ = util.GetPasswordFromSession(ctx)
	data.Board.Actor = actor
	data.Board.Post.Actor = actor.Id
	data.Board.Restricted = actor.Restricted
	data.NewsItems = make([]db.NewsItem, 1)

	data.NewsItems[0], err = db.GetNewsItem(ts)
	if err != nil {
		return util.MakeError(err, "NewsGet")
	}

	data.Title = actor.PreferredUsername + ": " + data.NewsItems[0].Title

	data.Meta.Description = data.PreferredUsername + " is a federated image board based on ActivityPub. The current version of the code running on the server is still a work-in-progress product, expect a bumpy ride for the time being. Get the server code here: https://git.fchannel.org."
	data.Meta.Url = data.Board.Actor.Id
	data.Meta.Title = data.Title

	data.Themes = &config.Themes
	data.ThemeCookie = route.GetThemeCookie(ctx)

	return ctx.Render("news", fiber.Map{"page": data}, "layouts/main")
}

func NewsGetAll(ctx *fiber.Ctx) error {
	actor, err := activitypub.GetActorFromDB(config.Domain)
	if err != nil {
		return util.MakeError(err, "NewsGetAll")
	}

	var data route.PageData
	data.PreferredUsername = actor.PreferredUsername
	data.Title = actor.PreferredUsername + " News"
	data.Boards = webfinger.Boards
	data.Board.Name = ""
	data.Key = config.Key
	data.Board.Domain = config.Domain
	data.Board.ModCred, _ = util.GetPasswordFromSession(ctx)
	data.Board.Actor = actor
	data.Board.Post.Actor = actor.Id
	data.Board.Restricted = actor.Restricted

	data.NewsItems, err = db.GetNews(0)

	if err != nil {
		return util.MakeError(err, "NewsGetAll")
	}

	if len(data.NewsItems) == 0 {
		return ctx.Redirect("/", http.StatusSeeOther)
	}

	data.Meta.Description = data.PreferredUsername + " is a federated image board based on ActivityPub. The current version of the code running on the server is still a work-in-progress product, expect a bumpy ride for the time being. Get the server code here: https://git.fchannel.org."
	data.Meta.Url = data.Board.Actor.Id
	data.Meta.Title = data.Title

	data.Themes = &config.Themes
	data.ThemeCookie = route.GetThemeCookie(ctx)

	return ctx.Render("anews", fiber.Map{"page": data}, "layouts/main")
}

func NewsPost(ctx *fiber.Ctx) error {
	actor, err := activitypub.GetActorFromDB(config.Domain)

	if err != nil {
		return util.MakeError(err, "NewPost")
	}

	if has := actor.HasValidation(ctx); !has {
		return nil
	}

	var newsitem db.NewsItem

	newsitem.Title = ctx.FormValue("title")
	newsitem.Content = template.HTML(ctx.FormValue("summary"))

	if err := db.WriteNews(newsitem); err != nil {
		return util.MakeError(err, "NewPost")
	}

	return ctx.Redirect("/", http.StatusSeeOther)
}

func NewsDelete(ctx *fiber.Ctx) error {
	actor, err := activitypub.GetActorFromDB(config.Domain)

	if has := actor.HasValidation(ctx); !has {
		return nil
	}

	timestamp := ctx.Path()[13+len(config.Key):]

	tsint, err := strconv.Atoi(timestamp)

	if err != nil {
		return ctx.Status(404).Render("404", fiber.Map{})
	}

	if err := db.DeleteNewsItem(tsint); err != nil {
		return util.MakeError(err, "NewsDelete")
	}

	return ctx.Redirect("/news/", http.StatusSeeOther)
}

func GetNewsFeed(ctx *fiber.Ctx) error {
	feedtype := ctx.Params("feedtype")
	actor, err := activitypub.GetActorFromDB(config.Domain)
	if err != nil {
		return util.MakeError(err, "NewsGetAll")
	}
	now := time.Now()
	feed := &feeds.Feed{
		Title:   actor.PreferredUsername + " News",
		Link:    &feeds.Link{Href: config.Domain + "/news"},
		Created: now,
	}

	news, err := db.GetNews(0)
	if err != nil {
		return util.MakeError(err, "NewsFeed")
	}

	for _, item := range news {
		feedItem := &feeds.Item{
			Id:          config.Domain + "/news/" + strconv.Itoa(item.Time),
			Title:       item.Title,
			Link:        &feeds.Link{Href: config.Domain + "/news/" + strconv.Itoa(item.Time)},
			Description: string(item.Content),
			Created:     time.Unix(int64(item.Time), 0),
		}
		feed.Add(feedItem)
	}

	var feedContent string
	switch feedtype {
	case "atom":
		feedContent, err = feed.ToAtom()
		ctx.Set("Content-Type", "application/atom+xml")
	case "rss":
		feedContent, err = feed.ToRss()
		ctx.Set("Content-Type", "application/rss+xml")
	case "json":
		feedContent, err = feed.ToJSON()
		ctx.Set("Content-Type", "application/json")
	default:
		return ctx.Status(400).SendString("Invalid feed type")
	}

	if err != nil {
		return util.MakeError(err, "NewsFeed")
	}

	return ctx.SendString(feedContent)
}
