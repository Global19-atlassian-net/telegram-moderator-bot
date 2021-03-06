package main

import (
	"encoding/json"
	"fmt"
	"github.com/cosmos/telegram-moderator-bot/context"
	"github.com/cosmos/telegram-moderator-bot/db"
	"github.com/cosmos/telegram-moderator-bot/defaults"
	"github.com/cosmos/telegram-moderator-bot/telegram"
	"github.com/gorilla/mux"
	"log"
	"net/http"
	"strings"
)

// Available administrator commands help text. X will be replaced with backtick.
const textHelpAdminCommands = `
X/promoteX _@username_ - Promote a user to moderator.
X/demoteX _@username_ - Demote a moderator to user.`

// Available moderator commands help text. X will be replaced with backtick.
const textHelpModeratorCommands = `
X/banX _@username_ - Kick and ban a user.
X/unbanX _@username_ - Unban a user.
X/warnX _@username_ - Warn a user.
X/listX - List moderators.`

// Composed help text.
const textHelpMessage = `Hi %s!
You are a%s.

Available commands:%s%s`

// Template for list-like messages.
const textListMessage = `%s:
%s.`

// String-joiner text.
const textNewlineComma = `,
`

// MembersType types
const (
	regular = iota
	moderators
	admininstrators
	creator
	kicked
	left
)

// Structure to hold parsed incoming text.
type CommandData struct {
	Command     string
	Users       []*telegram.User
	UserStrings []string
}

// Filters incoming messages and updates internal database with user IDs. Filters out bots.
func PreprocessMessage(ctx *context.Context, incoming *telegram.Update) (message *telegram.Message) {
	var from *telegram.User = nil

	switch {
	case incoming.Message != nil:
		message = incoming.Message
		from = message.From
	case incoming.EditedMessage != nil:
		message = incoming.EditedMessage
		from = message.From
	case incoming.ChannelPost != nil:
		from = incoming.ChannelPost.From
	case incoming.EditedChannelPost != nil:
		from = incoming.EditedChannelPost.From
	case incoming.InlineQuery != nil:
		from = incoming.InlineQuery.From
	case incoming.ChosenInlineResult.From != nil:
		from = incoming.ChosenInlineResult.From
	case incoming.CallbackQuery != nil:
		from = incoming.CallbackQuery.From
	case incoming.PreCheckoutQuery != nil:
		from = incoming.PreCheckoutQuery.From
	case incoming.ShippingQuery != nil:
		from = incoming.ShippingQuery.From
	default:
		return
	}

	if from.IsBot {
		return
	}

	name := from.FirstName
	if from.LastName != "" {
		name = name + " " + from.LastName
	}

	err := db.UpdateUserData(ctx, &db.UserData{from.Username, from.Id, name})
	if err != nil {
		//Todo: handle DynamoDB capacity limitations
		log.Printf("[tempdebug] error updating user in DB: %+v", err.Error())
	}

	return
}

// Parse the incoming message for bot command and a list of users.
func ParseInput(m *telegram.Message) *CommandData {
	output := &CommandData{}

	if len(m.Entities) < 1 {
		return nil
	}

	if m.Text[0] != '/' {
		return nil
	}

	if m.Entities[0].Type != "bot_command" {
		return nil
	} else {
		output.Command = m.Text[m.Entities[0].Offset : m.Entities[0].Offset+m.Entities[0].Length]
	}
	for _, entity := range m.Entities {
		if entity.Type == "text_mention" {
			output.Users = append(output.Users, entity.User)
		} else {
			if entity.Type == "mention" {
				//Cut off the "@" from the front of the username.
				output.UserStrings = append(output.UserStrings, m.Text[entity.Offset+1:entity.Offset+entity.Length])
			}
		}
	}

	return output
}

// Checks the list of members and compiles a User array out of valid users.
func CheckMembers(ctx *context.Context, ChatId int64, command *CommandData, MembersType int) []*telegram.User {
	var ids []int
	var result []*telegram.User

	for _, user := range command.Users {
		ids = append(ids, user.Id)
	}

	for _, user := range command.UserStrings {
		dbUserData, err := db.GetUserData(ctx, user)
		if err != nil {
			if defaults.Debug {
				log.Printf("[debug] (CheckMembers) Could not get user data from database for user %s, %+v", user, err.Error())
			}
			continue
		}
		if dbUserData == nil {
			if defaults.Debug {
				log.Printf("[debug] (CheckMembers) User not found in database: %s", user)
			}
			continue
		}

		ids = append(ids, dbUserData.UserID)
	}

	for _, userId := range ids {
		userData, err := telegram.GetChatMember(ctx, ChatId, userId)
		if err != nil {
			if defaults.Debug {
				log.Printf("[debug] (CheckMembers) Could not get user verification data from Telegram for user ID %d, %+v", userId, err.Error())
			}
			continue
		}

		if userData.User.IsBot {
			continue
		}

		if defaults.Debug {
			log.Printf("[debug] CheckMembers ChatMember %+v.", userData)
		}

		if MembersType == regular {
			if userData.Status != "member" {
				continue
			}
		}

		if MembersType == creator {
			if userData.Status != "creator" {
				continue
			}
		}

		if MembersType == kicked {
			if userData.Status != "kicked" {
				continue
			}
		}

		if MembersType == left {
			if userData.Status != "left" {
				continue
			}
		}

		if MembersType == moderators {
			if userData.Status != "administrator" || userData.CanPromoteMembers {
				continue
			}
		}

		if MembersType == admininstrators {
			if (userData.Status != "administrator" || !userData.CanPromoteMembers) && userData.Status != "creator" {
				continue
			}
		}

		if defaults.Debug {
			log.Printf("[debug] CheckMembers Keeping ChatMember %+v.", userData)
		}

		result = append(result, userData.User)
	}
	return result
}

// MainHandler handles the requests coming to `/`.
func MainHandler(ctx *context.Context, w http.ResponseWriter, r *http.Request) (status int, err error) {
	status = http.StatusOK
	w.WriteHeader(status)

	incoming := &telegram.Update{}
	err = json.NewDecoder(r.Body).Decode(incoming)
	if err != nil {
		log.Printf("[error] MainHandler decoder: %v", err)
		status = http.StatusBadRequest
		return
	}

	if incoming == nil {
		log.Print("[error] MainHandler incoming data is empty.")
		status = http.StatusBadRequest
		return
	}

	message := PreprocessMessage(ctx, incoming)

	if message == nil {
		return
	}

	if message.Chat.Type != "supergroup" {
		return
	}

	command := ParseInput(message)
	if command == nil {
		return
	}

	chatId := message.Chat.Id
	messageId := message.MessageId

	if defaults.Debug {
		log.Printf("[debug] Command received %s from %s. Mentions: %s, Text_Mentions: %+v.", command.Command, message.From, strings.Join(command.UserStrings, ";"), command.Users)
		log.Printf("[debug] Chat ID: %d, Message ID: %d, User ID: %d", chatId, messageId, message.From.Id)
	}

	isAdmin, isMod, getPrivilegesError := telegram.GetPrivileges(ctx, chatId, message.From.Id)
	if getPrivilegesError != nil {
		telegram.ReplyMessage(ctx, chatId, messageId, "Could not check user privileges.")
		return status, getPrivilegesError
	}

	if !isMod {
		return
	}

	// Commands for moderators
	switch command.Command {
	case "/help":
		adminCommands := ""
		privilegeSnippet := " *moderator*"
		if isAdmin {
			privilegeSnippet = "n *administrator*"
			adminCommands = textHelpAdminCommands
		}

		text := fmt.Sprintf(textHelpMessage,
			message.From.FirstName,
			privilegeSnippet,
			strings.Replace(textHelpModeratorCommands, "X", "`", -1),
			strings.Replace(adminCommands, "X", "`", -1))
		telegram.ReplyMessage(ctx, chatId, messageId, text)
		return
	case "/warn":
		warned, banned := telegram.WarnMember(ctx, chatId, CheckMembers(ctx, chatId, command, regular))
		if len(warned) >= 1 {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf(textListMessage, "Warned user(s)", strings.Join(warned, textNewlineComma)))
		}
		if len(banned) >= 1 {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf(textListMessage, "Banned user(s)", strings.Join(banned, textNewlineComma)))
		}
		if len(warned) < 1 && len(banned) < 1 {
			telegram.ReplyMessage(ctx, chatId, messageId, "No users were warned.")
		}
		return
	case "/ban":
		list := telegram.BanMember(ctx, chatId, CheckMembers(ctx, chatId, command, regular))
		if len(list) < 1 {
			telegram.ReplyMessage(ctx, chatId, messageId, "No users were banned.")
		} else {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf(textListMessage, "Banned user(s)", strings.Join(list, textNewlineComma)))
		}
		return
	case "/unban":
		list := telegram.UnbanMember(ctx, chatId, CheckMembers(ctx, chatId, command, kicked))
		if len(list) < 1 {
			telegram.ReplyMessage(ctx, chatId, messageId, "No users were unbanned.")
		} else {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf(textListMessage, "Unbanned user(s)", strings.Join(list, textNewlineComma)))
		}
		return
	case "/list":
		list := telegram.ListModerators(ctx, chatId)
		if len(list) < 1 {
			telegram.ReplyMessage(ctx, chatId, messageId, "No moderators found.")
		} else {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf(textListMessage, "Moderators", strings.Join(list, textNewlineComma)))
		}
		return
	}

	if !isAdmin {
		log.Printf("[warning] Non-administrator trying administrator command: %s, %s", command.Command, message.From)
		return
	}

	// Commands for administrators
	switch command.Command {
	case "/promote":
		list, errors := telegram.AddModerator(ctx, chatId, CheckMembers(ctx, chatId, command, regular))
		if len(list) < 1 {
			telegram.ReplyMessage(ctx, chatId, messageId, "No moderators were added.")
		} else {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf(textListMessage, "Added moderator(s)", strings.Join(list, textNewlineComma)))
		}
		if len(errors) > 0 {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf("Errors: %s.", strings.Join(errors, "; ")))
		}
	case "/demote":
		list, errors := telegram.RemoveModerator(ctx, chatId, CheckMembers(ctx, chatId, command, moderators))
		if len(list) < 1 {
			telegram.ReplyMessage(ctx, chatId, messageId, "No moderators were removed.")
		} else {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf(textListMessage, "Removed moderator(s)", strings.Join(list, textNewlineComma)))
		}
		if len(errors) > 0 {
			telegram.ReplyMessage(ctx, chatId, messageId, fmt.Sprintf("Errors: %s.", strings.Join(errors, "; ")))
		}
	}

	return
}

// AddRoutes adds the routes of the different calls to GorillaMux.
func AddRoutes(ctx *context.Context) (r *mux.Router) {

	// Root and routes
	r = mux.NewRouter()
	r.Handle("/", context.Handler{ctx, MainHandler})

	// Finally
	http.Handle("/", r)

	return
}
