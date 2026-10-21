// Package telegram is a minimal client of the Telegram Bot API: the calls
// and fields the bridge uses, nothing more.
package telegram

// Update is one incoming event.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// User is a Telegram account.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// Name is the display name.
func (u *User) Name() string {
	if u == nil {
		return ""
	}
	if u.LastName != "" {
		return u.FirstName + " " + u.LastName
	}
	return u.FirstName
}

// Chat is a chat, group or channel.
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"` // private | group | supergroup | channel
	Title     string `json:"title,omitempty"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// Name is the title or the person's name.
func (c *Chat) Name() string {
	if c == nil {
		return ""
	}
	if c.Title != "" {
		return c.Title
	}
	if c.LastName != "" {
		return c.FirstName + " " + c.LastName
	}
	return c.FirstName
}

// Entity is a styled span (offsets in UTF-16 code units).
type Entity struct {
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	URL      string `json:"url,omitempty"`
	Language string `json:"language,omitempty"`
	User     *User  `json:"user,omitempty"`
}

// MessageOrigin says where a forwarded message comes from.
type MessageOrigin struct {
	Type            string `json:"type"` // user | hidden_user | chat | channel
	Date            int64  `json:"date"`
	SenderUser      *User  `json:"sender_user,omitempty"`
	SenderUserName  string `json:"sender_user_name,omitempty"`
	SenderChat      *Chat  `json:"sender_chat,omitempty"`
	Chat            *Chat  `json:"chat,omitempty"`
	MessageID       int64  `json:"message_id,omitempty"`
	AuthorSignature string `json:"author_signature,omitempty"`
}

// PhotoSize is one size of a photo.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size,omitempty"`
}

// FileBase is common to the file kinds.
type FileBase struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileName string `json:"file_name,omitempty"`
	Duration int    `json:"duration,omitempty"`
}

// Audio is a music file.
type Audio struct {
	FileBase
	Performer string `json:"performer,omitempty"`
	Title     string `json:"title,omitempty"`
}

// Sticker (not bridged).
type Sticker struct {
	FileID string `json:"file_id"`
	Emoji  string `json:"emoji,omitempty"`
}

// PollOption is one answer.
type PollOption struct {
	Text string `json:"text"`
}

// Poll is a Telegram poll.
type Poll struct {
	Question string       `json:"question"`
	Options  []PollOption `json:"options"`
}

// Location is a point.
type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// Venue is a named place.
type Venue struct {
	Location Location `json:"location"`
	Title    string   `json:"title"`
	Address  string   `json:"address"`
}

// Contact is a shared phone number.
type Contact struct {
	PhoneNumber string `json:"phone_number"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name,omitempty"`
	UserID      int64  `json:"user_id,omitempty"`
}

// Message is a message.
type Message struct {
	MessageID       int64          `json:"message_id"`
	From            *User          `json:"from,omitempty"`
	Chat            Chat           `json:"chat"`
	Date            int64          `json:"date"`
	MediaGroupID    string         `json:"media_group_id,omitempty"`
	ForwardOrigin   *MessageOrigin `json:"forward_origin,omitempty"`
	Text            string         `json:"text,omitempty"`
	Entities        []Entity       `json:"entities,omitempty"`
	Caption         string         `json:"caption,omitempty"`
	CaptionEntities []Entity       `json:"caption_entities,omitempty"`
	Photo           []PhotoSize    `json:"photo,omitempty"`
	Video           *FileBase      `json:"video,omitempty"`
	Animation       *FileBase      `json:"animation,omitempty"`
	Document        *FileBase      `json:"document,omitempty"`
	Audio           *Audio         `json:"audio,omitempty"`
	Voice           *FileBase      `json:"voice,omitempty"`
	VideoNote       *FileBase      `json:"video_note,omitempty"`
	Sticker         *Sticker       `json:"sticker,omitempty"`
	Poll            *Poll          `json:"poll,omitempty"`
	Location        *Location      `json:"location,omitempty"`
	Venue           *Venue         `json:"venue,omitempty"`
	Contact         *Contact       `json:"contact,omitempty"`
}

// CallbackQuery is an inline button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// File is the answer of getFile.
type File struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size,omitempty"`
	FilePath string `json:"file_path,omitempty"`
}

// InlineButton is a button under a message.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// InlineKeyboard is reply_markup with buttons under the message.
type InlineKeyboard struct {
	InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
}

// KeyboardButton is a button of the reply keyboard.
type KeyboardButton struct {
	Text           string `json:"text"`
	RequestContact bool   `json:"request_contact,omitempty"`
}

// ReplyKeyboard replaces the person's keyboard.
type ReplyKeyboard struct {
	Keyboard        [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard  bool               `json:"resize_keyboard,omitempty"`
	OneTimeKeyboard bool               `json:"one_time_keyboard,omitempty"`
}

// RemoveKeyboard removes a reply keyboard.
type RemoveKeyboard struct {
	RemoveKeyboard bool `json:"remove_keyboard"`
}

// BotCommand is one entry of the command menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}
