package main

import (
	"fmt"
	"html"
	"log"
	"os"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"

	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

type OpenRouterRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Структура для хранения данных пользователя
type UserData struct {
    MessageTimes []time.Time
    ChatHistory  []Message  
    mu           sync.Mutex
}

const defaultModel = "arcee-ai/trinity-large-preview:free"
//const defaultModel = "deepseek/deepseek-v3.2"  tngtech/deepseek-r1t2-chimera:free.  arcee-ai/trinity-large-preview:free

// Хранилище данные юзеров
var users = make(map[int64]*UserData)
var usersMutex sync.RWMutex

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	botToken := os.Getenv("TELEGRAM_TOKEN")
	if botToken == "" {
		log.Fatal("TELEGRAM_TOKEN is not set in .env file")
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Panic(err)
	}

	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		// Обработка команд
		if update.Message != nil && update.Message.IsCommand() {
			switch update.Message.Command() {
			case "start":
				sendStartMessage(bot, update.Message.Chat.ID)
			default:
				sendMessage(bot, update.Message.Chat.ID, "Неизвестная команда. Используйте /start")
			}
			continue
		}

		// Обработка обычных сообщений
		if update.Message != nil && update.Message.Text != "" {
			userID := update.Message.From.ID

			// Проверка rate limit
			if !checkRateLimit(userID) {
				sendMessage(bot, update.Message.Chat.ID, "⏱️ Слишком много сообщений. Лимит: 10 сообщений в минуту")
				continue
			}

			log.Printf("[%s] %s", update.Message.From.UserName, update.Message.Text)

			// Отправляем сообщение пользователя в AI
			response, err := getAIResponse(update.Message.Text, userID)
			if err != nil {
				log.Printf("Error getting AI response: %v", err)
				sendMessage(bot, update.Message.Chat.ID, fmt.Sprintf("❌ Ошибка: %v", err))
				continue
			}
			response = html.EscapeString(response)
			msg := tgbotapi.NewMessage(update.Message.Chat.ID, response)
			msg.ParseMode = "HTML"
			bot.Send(msg)
		}

	}
}

func getAIResponse(prompt string, userID int64) (string, error) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("OPENROUTER_API_KEY must be set in .env")
	}

	userData := getUserData(userID)
	userData.mu.Lock()
	defer userData.mu.Unlock()

	// Определяем системный промпт в зависимости от пользователя
	var systemPrompt string
	if userID == 853329884 {
		systemPrompt = "Отвечай как интеллигентный и саркастичный собеседник, не слишком удлинняя ответ, собеседника зовут Ханифа, ей 23 года, она окончила РУДН на психолога криминалиста"
	} else {
		systemPrompt = "Отвечай кратко, по делу, без воды."
	}

	// Если история пустая, добавляем системный промпт
	if len(userData.ChatHistory) == 0 {
		userData.ChatHistory = append(userData.ChatHistory, Message{
			Role:    "system",
			Content: systemPrompt,
		})
	}

	// Добавляем сообщение пользователя в историю
	userData.ChatHistory = append(userData.ChatHistory, Message{
		Role:    "user",
		Content: prompt,
	})

	// Ограничиваем историю (последние 20 сообщений + системный промпт)
	maxHistory := 21 // system + 20 сообщений
	if len(userData.ChatHistory) > maxHistory {
		// Сохраняем системный промпт (первое сообщение) + последние N сообщений
		userData.ChatHistory = append(
			[]Message{userData.ChatHistory[0]}, 
			userData.ChatHistory[len(userData.ChatHistory)-maxHistory+1:]...,
		)
	}

	// Формируем запрос с полной историей
	requestBody := OpenRouterRequest{
		Model:    defaultModel,
		Messages: userData.ChatHistory,
	}

	jsonData, _ := json.Marshal(requestBody)
	req, _ := http.NewRequest("POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewBuffer(jsonData))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	json.Unmarshal(body, &result)

	// Проверяем наличие ошибки в ответе
	if errMsg, ok := result["error"]; ok {
		return "", fmt.Errorf("API error: %v", errMsg)
	}

	// Проверяем, есть ли 'choices' и не пустой ли он
	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("no choices returned from API. Response: %s", string(body)[:100])
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid choice format")
	}

	message, ok := choice["message"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid message format")
	}

	content, ok := message["content"].(string)
	if !ok {
		return "", fmt.Errorf("no content in message")
	}

	// Добавляем ответ AI в историю
	userData.ChatHistory = append(userData.ChatHistory, Message{
		Role:    "assistant",
		Content: content,
	})

	return content, nil
}

// Функции для управления моделями и rate limiting
func getUserData(userID int64) *UserData {
	usersMutex.RLock()
	if user, exists := users[userID]; exists {
		usersMutex.RUnlock()
		return user
	}
	usersMutex.RUnlock()

	// Создаем новые данные пользователя
	userData := &UserData{
		MessageTimes: []time.Time{},
	}

	usersMutex.Lock()
	users[userID] = userData
	usersMutex.Unlock()

	return userData
}

// Rate limit: 10 сообщений в минуту
func checkRateLimit(userID int64) bool {
	userData := getUserData(userID)
	userData.mu.Lock()
	defer userData.mu.Unlock()

	now := time.Now()
	oneMinuteAgo := now.Add(-1 * time.Minute)

	// Удаляем сообщения старше минуты
	validMessages := []time.Time{}
	for _, t := range userData.MessageTimes {
		if t.After(oneMinuteAgo) {
			validMessages = append(validMessages, t)
		}
	}
	userData.MessageTimes = validMessages

	// Проверяем лимит
	if len(userData.MessageTimes) >= 10 {
		return false
	}

	// Добавляем текущее сообщение
	userData.MessageTimes = append(userData.MessageTimes, now)
	return true
}

func sendStartMessage(bot *tgbotapi.BotAPI, chatID int64) {
	text := "👋 Привет!"
	sendMessage(bot, chatID, text)
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	bot.Send(msg)
}
