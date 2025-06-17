package main

import (
	"encoding/json"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LightUpProgram struct {
	ID             uint `gorm:"primaryKey"`
	Name           string
	Description    string
	LightupProgram int64         `gorm:"unique"`
	Steps          []LightUpStep `gorm:"foreignKey:ProgramID;constraint:OnDelete:CASCADE"`
}

type LightUpStep struct {
	ID         uint `gorm:"primaryKey"`
	StepNo     int
	DelayMin   int
	ProgramID  uint
	LightPower int
}

type Lantern struct {
	ID               uint `gorm:"primaryKey"`
	State            int  `gorm:"check:state IN (0,1,2)"`
	LightPower       int  `gorm:"check:light_power BETWEEN 0 AND 255"`
	UpdatedAt        time.Time
	LightUpProgramID *uint
	LightUpProgram   *LightUpProgram `gorm:"foreignKey:LightUpProgramID;constraint:OnDelete:SET NULL"`
	InstallationDate time.Time       `gorm:"column:installation_date"`
}

var (
	db            *gorm.DB
	mqttClient    mqtt.Client
	programMux    sync.Mutex
	initialStates = make(map[uint]LanternState) // Map to store initial states
	stateMutex    sync.Mutex
)

type LanternState struct {
	State      int
	LightPower int
}

func initMQTT() {
	broker := os.Getenv("MQTT_BROKER")
	username := os.Getenv("MQTT_USER")
	clientID := os.Getenv("CLIENT_ID")
	password := os.Getenv("MQTT_PASSWORD")

	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetUsername(username).
		SetPassword(password)

	opts.OnConnect = func(client mqtt.Client) {
		log.Println("Connected to MQTT Broker")
	}

	opts.OnConnectionLost = func(client mqtt.Client, err error) {
		log.Printf("Connection lost: %v\n", err)
	}

	mqttClient = mqtt.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Ошибка подключения к MQTT: %v", token.Error())
	}

}

func initGORM() {
	dsn := fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%s sslmode=disable",
		os.Getenv("DB_HOST"),
		os.Getenv("DB_USER"),
		os.Getenv("DB_PASSWORD"),
		os.Getenv("DB_NAME"),
		os.Getenv("DB_PORT"),
	)

	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	})
	if err != nil {
		log.Fatalf("Ошибка подключения к PostgreSQL: %v", err)
	}

	// Автомиграция
	if err := db.AutoMigrate(&LightUpProgram{}, &LightUpStep{}, &Lantern{}); err != nil {
		log.Fatalf("Ошибка миграции: %v", err)
	}

	log.Println("Successfully connected to PostgreSQL")
}

func handleStatus(_ mqtt.Client, msg mqtt.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) < 3 {
		return
	}

	lampID, err := strconv.Atoi(parts[1])
	if err != nil || lampID < 1 {
		log.Printf("Некорректный ID светильника: %s", parts[1])
		return
	}

	var payload struct {
		Status     string `json:"state"`
		Brightness int    `json:"brightness"`
	}
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		log.Printf("Ошибка JSON: %v", err)
		return
	}

	log.Printf("Лампа %d: state=%s brightness=%d", lampID, payload.Status, payload.Brightness)

	// Получаем текущее состояние лампы из БД
	var lantern Lantern
	result := db.First(&lantern, lampID)
	if result.Error != nil {
		log.Printf("Ошибка поиска светильника %d: %v", lampID, result.Error)
		return
	}

	needUpdate := false
	originalState := lantern.State // Сохраняем исходный режим

	if payload.Status == "off" {
		if lantern.State != 0 { // Если лампа не была выключена
			lantern.State = 0
			needUpdate = true
		}
	} else { // status == "on"
		if lantern.State == 0 { // Если лампа была выключена
			// Возвращаем в предыдущий режим (1 - программа, 2 - кастомный)
			lantern.State = originalState
			needUpdate = true
		}

		// Обновляем яркость, если она изменилась
		if lantern.LightPower != payload.Brightness {
			lantern.LightPower = payload.Brightness
			needUpdate = true
		}
	}

	if needUpdate {
		lantern.UpdatedAt = time.Now()
		if err := db.Save(&lantern).Error; err != nil {
			log.Printf("Ошибка сохранения светильника %d: %v", lampID, err)
		}
	}
}

func seasonChecker() {
	currentSeason := ""
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		month := time.Now().Month()
		season := "winter"
		if month >= time.June && month <= time.August {
			season = "summer"
		}

		if season != currentSeason {
			currentSeason = season
			log.Printf("Смена сезона: %s", season)
			applySeasonProgram(season)
		}
	}
}

func applyInitialProgram() {
	time.Sleep(5 * time.Second) // Ждем инициализации
	month := time.Now().Month()
	season := "winter"
	if month >= time.June && month <= time.August {
		season = "summer"
	}
	applySeasonProgram(season)
}
func applySeasonProgram(season string) {
	programMux.Lock()
	defer programMux.Unlock()

	// Определяем ID программы по сезону
	programID := uint(2) // по умолчанию зимняя
	if season == "summer" {
		programID = 1
	}

	// Обновляем все лампы, устанавливая им программу по сезону
	if err := db.Model(&Lantern{}).Where("1 = 1").Update("light_up_program_id", programID).Error; err != nil {
		log.Printf("Ошибка обновления программ ламп: %v", err)
	}

	// Находим программу в базе
	var program LightUpProgram
	result := db.Where("id = ?", programID).First(&program)
	if result.Error != nil {
		log.Printf("Программа '%d' не найдена: %v", programID, result.Error)
		return
	}

	// Загружаем шаги программы
	var steps []LightUpStep
	db.Where("program_id = ?", program.ID).Order("step_no").Find(&steps)
	if len(steps) == 0 {
		log.Printf("Для программы '%d' нет шагов", programID)
		return
	}

	log.Printf("Применение программы '%s' (%d шагов)", season, len(steps))
	for _, step := range steps {
		applyStepToAllLamps(step.LightPower)
		time.Sleep(time.Duration(step.DelayMin) * time.Second)
	}

	finalizeLightUpProgram()
}

func applyStepToAllLamps(brightness int) {
	// Получаем все светильники из базы данных
	var lanterns []Lantern

	db.Where("State = ?", 1).Find(&lanterns)

	if len(lanterns) == 0 {
		log.Println("В базе данных нет светильников")
		return
	}

	// Применяем яркость ко всем светильникам

	for _, lantern := range lanterns {
		topic := fmt.Sprintf("virtual/%d/brightness/set", lantern.ID)
		token := mqttClient.Publish(topic, 0, false, strconv.Itoa(brightness))
		token.Wait()

		// Обновляем состояние в БД
		db.Model(&lantern).Updates(map[string]interface{}{
			"state":       1,
			"light_power": brightness,
		})
	}

	log.Printf("Установлена яркость %d для %d светильников", brightness, len(lanterns))
}

func handleCustomCommand(_ mqtt.Client, msg mqtt.Message) {
	parts := strings.Split(msg.Topic(), "/")
	if len(parts) < 3 {
		return
	}

	lampID, err := strconv.Atoi(parts[1])
	if err != nil || lampID < 1 {
		log.Printf("Некорректный ID светильника: %s", parts[1])
		return
	}

	// Парсим JSON с кастомными параметрами
	var payload struct {
		Brightness int `json:"brightness"`
		State      int `json:"state"`
	}
	if err := json.Unmarshal(msg.Payload(), &payload); err != nil {
		log.Printf("Ошибка JSON: %v", err)
		return
	}

	log.Printf("Кастомная команда для лампы %d: brightness=%d", lampID, payload.Brightness)

	// Устанавливаем кастомные параметры
	if err := setCustomLightSettings(uint(lampID), payload.Brightness, payload.State); err != nil {
		log.Printf("Ошибка установки кастомных параметров: %v", err)
	}
}

// Функция для установки кастомных параметров светильника
func setCustomLightSettings(lampID uint, brightness int, state int) error {
	// Проверяем корректность яркости
	if brightness < 0 || brightness > 255 {
		return fmt.Errorf("некорректное значение яркости: %d", brightness)
	}

	// Находим светильник
	var lantern Lantern
	result := db.First(&lantern, lampID)
	if result.Error != nil {
		return fmt.Errorf("светильник %d не найден: %v", lampID, result.Error)
	}

	// Обновляем параметры
	lantern.State = state // Кастомный режим
	lantern.LightPower = brightness
	lantern.UpdatedAt = time.Now()

	// Сохраняем в БД
	if err := db.Save(&lantern).Error; err != nil {
		return fmt.Errorf("ошибка сохранения: %v", err)
	}

	// Отправляем команду на физический светильник
	topic := fmt.Sprintf("virtual/%d/brightness/set", lampID)
	token := mqttClient.Publish(topic, 0, false, strconv.Itoa(brightness))
	token.Wait()

	if token.Error() != nil {
		return fmt.Errorf("ошибка MQTT: %v", token.Error())
	}

	log.Printf("Установлен кастомный режим для лампы %d: яркость %d", lampID, brightness)
	return nil
}
func finalizeLightUpProgram() {
	programMux.Lock()
	defer programMux.Unlock()

	var lanterns []Lantern
	if err := db.Where("state != ?", 0).Find(&lanterns).Error; err != nil {
		log.Printf("Ошибка получения активных ламп: %v", err)
		return
	}

	for _, lantern := range lanterns {
		topic := fmt.Sprintf("virtual/%d/brightness/set", lantern.ID)
		token := mqttClient.Publish(topic, 0, false, "0")
		token.Wait()
	}

	// Обновляем все в БД
	if err := db.Model(&Lantern{}).Where("state != ?", 0).Updates(map[string]interface{}{
		"state":       0,
		"light_power": 0,
	}).Error; err != nil {
		log.Printf("Ошибка обновления ламп: %v", err)
	} else {
		log.Println("LightUpProgram завершён: все лампы выключены (MQTT + БД)")
	}
}

func handleCarDetection(_ mqtt.Client, msg mqtt.Message) {
	log.Println("Car detected - activating special lighting mode")

	// 1. Store initial states of all lanterns
	storeInitialStates()

	// 2. Set all lanterns to max brightness (255) and state 2 (custom mode)
	setAllLanternsToDetectionMode()

	// 3. After 30 seconds, restore initial states
	time.AfterFunc(30*time.Second, restoreInitialStates)
}

func storeInitialStates() {
	stateMutex.Lock()
	defer stateMutex.Unlock()

	var lanterns []Lantern
	if err := db.Find(&lanterns).Error; err != nil {
		log.Printf("Error fetching lantern states: %v", err)
		return
	}

	initialStates = make(map[uint]LanternState)
	for _, l := range lanterns {
		initialStates[l.ID] = LanternState{
			State:      l.State,
			LightPower: l.LightPower,
		}
	}
	log.Printf("Stored initial states for %d lanterns", len(initialStates))
}

func setAllLanternsToDetectionMode() {
	var lanterns []Lantern
	if err := db.Find(&lanterns).Error; err != nil {
		log.Printf("Error fetching lanterns: %v", err)
		return
	}

	for _, lantern := range lanterns {
		// Update in database
		if err := db.Model(&lantern).Updates(map[string]interface{}{
			"state":       2, // Custom mode
			"light_power": 255,
			"updated_at":  time.Now(),
		}).Error; err != nil {
			log.Printf("Error updating lantern %d: %v", lantern.ID, err)
			continue
		}

		// Send MQTT command
		topic := fmt.Sprintf("virtual/%d/custom/set", lantern.ID)
		payload := fmt.Sprintf(`{"brightness": 255, "state": 2}`)
		token := mqttClient.Publish(topic, 0, false, payload)
		token.Wait()

		if token.Error() != nil {
			log.Printf("MQTT error for lantern %d: %v", lantern.ID, token.Error())
		}
	}
	log.Println("All lanterns set to detection mode (brightness=255, state=2)")
}

func restoreInitialStates() {
	stateMutex.Lock()
	defer stateMutex.Unlock()

	if len(initialStates) == 0 {
		log.Println("No initial states to restore")
		return
	}

	for id, state := range initialStates {
		// Update in database
		if err := db.Model(&Lantern{}).Where("id = ?", id).Updates(map[string]interface{}{
			"state":       state.State,
			"light_power": state.LightPower,
			"updated_at":  time.Now(),
		}).Error; err != nil {
			log.Printf("Error restoring lantern %d: %v", id, err)
			continue
		}

		// Send MQTT command
		topic := fmt.Sprintf("virtual/%d/custom/set", id)
		payload := fmt.Sprintf(`{"brightness": %d, "state": %d}`, state.LightPower, state.State)
		token := mqttClient.Publish(topic, 0, false, payload)
		token.Wait()

		if token.Error() != nil {
			log.Printf("MQTT error restoring lantern %d: %v", id, token.Error())
		}
	}
	log.Printf("Restored initial states for %d lanterns", len(initialStates))
}

func main() {
	// Загрузка .env
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: .env file not loaded")
	}

	// Инициализация MQTT
	initMQTT()
	defer mqttClient.Disconnect(250)

	// Инициализация GORM
	initGORM()

	// Подписка на MQTT
	if token := mqttClient.Subscribe("virtual/+/status", 0, handleStatus); token.Wait() && token.Error() != nil {
		log.Fatalf("Ошибка подписки: %v", token.Error())
	}

	if token := mqttClient.Subscribe("virtual/+/custom/set", 0, handleCustomCommand); token.Wait() && token.Error() != nil {
		log.Fatalf("Ошибка подписки на кастомные команды: %v", token.Error())
	}

	if token := mqttClient.Subscribe("car/detected", 0, handleCarDetection); token.Wait() && token.Error() != nil {
		log.Fatalf("Error subscribing to car detection: %v", token.Error())
	}

	// Запуск фоновых задач
	go seasonChecker()
	go applyInitialProgram()

	log.Println("Сервис запущен. Ожидание сообщений...")
	select {}
}
