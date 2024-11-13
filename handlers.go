package main

import (
    "crypto/tls"
    "encoding/csv"
    "fmt"
    "github.com/PuerkitoBio/goquery"
    "github.com/bwmarrin/discordgo"
    "io"
    "log"
    "net/http"
    "net/http/cookiejar"
    "os"
    "os/exec"
    "strings"
    "time"
)

func handleReady(s *discordgo.Session, r *discordgo.Ready) {
    s.UpdateStatusComplex(discordgo.UpdateStatusData{
        Activities: []*discordgo.Activity{
            {
                Name: "linktr.ee/calpolyswift",
                Type: discordgo.ActivityTypeWatching,
            },
        },
        Status: "online",
        AFK:    false,
    })
}

func handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
    if i.GuildID == "" {
        log.Println("Ignoring DM interaction")
        return
    }

    if i.Type == discordgo.InteractionApplicationCommand {
        switch i.ApplicationCommandData().Name {
        case "multi":
            handleProfileCommand(s, i)
        case "single":
            handleSingleProfileCommand(s, i)
        case "display":
            handleDisplayCommand(s, i)
        case "conns":
            handleConnsCommand(s, i)
        case "delete":
            handleDeleteCommand(s, i)
        case "status":
            handleStatusCommand(s, i)
        case "kill":
            handleKillCommand(s, i)
        }
    }
}

func handleConnsCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
    err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: "Fetching VPN connections...",
        },
    })
    if err != nil {
        log.Printf("Failed to send initial response: %v", err)
        return
    }

    jar, _ := cookiejar.New(nil)
    client := &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
        },
        Jar: jar,
    }

    loginSuccess, err := loginToPfSense(client)
    if err != nil || !loginSuccess {
        respondWithError(s, i, "Login to pfSense failed. Please try again.")
        return
    }

    vpnStatusURL := BaseURL + OpenvpnStatusPath
    resp, err := client.Get(vpnStatusURL)
    if err != nil {
        respondWithError(s, i, "Failed to fetch OpenVPN status page.")
        return
    }
    defer resp.Body.Close()

    doc, err := goquery.NewDocumentFromReader(resp.Body)
    if err != nil {
        respondWithError(s, i, "Failed to parse OpenVPN status page.")
        return
    }

    var vpnConns []string
    doc.Find("tr").Each(func(index int, row *goquery.Selection) {
        clientName := strings.TrimSpace(row.Find("td").Eq(0).Text())
        clientIP := strings.TrimSpace(row.Find("td").Eq(1).Text())

        if clientName != "" && clientIP != "" && strings.Contains(clientName, "mickey.sdc.cpp") && len(strings.Fields(clientName)) > 1 {
            clientInfo := strings.Join(strings.Fields(clientName), " ")
            vpnConns = append(vpnConns, fmt.Sprintf("%s - %s", clientInfo, clientIP))
        }
    })

    var messageContent string
    if len(vpnConns) == 0 {
        messageContent = "No active VPN connections found."
    } else {
        messageContent = strings.Join(vpnConns, "\n")
    }

    if len(messageContent) > 2000 {
        log.Printf("VPN connections output (over character limit):\n%s", messageContent)
        _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
            Content: "Output over character limit, check logs",
        })
    } else {
        _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
            Content: messageContent,
        })
    }

    if err != nil {
        log.Printf("Failed to send message: %v", err)
    }
}

func ensureBitwardenLogin() error {
    cmd := exec.Command("bw", "status")
    output, err := cmd.CombinedOutput()
    if err == nil && strings.Contains(string(output), "locked") {
        unlockCmd := exec.Command("bw", "unlock", "--raw")
        unlockCmd.Stdin = strings.NewReader(os.Getenv("BITWARDEN_MASTER_PASSWORD"))
        sessionKey, unlockErr := unlockCmd.Output()
        if unlockErr != nil {
            return fmt.Errorf("failed to unlock Bitwarden: %v", unlockErr)
        }
        os.Setenv("BW_SESSION", strings.TrimSpace(string(sessionKey)))
    } else if err != nil || strings.Contains(string(output), "unauthenticated") {
        loginCmd := exec.Command("bw", "login", "--apikey")
        loginCmd.Env = append(os.Environ(),
            "BW_CLIENTID="+os.Getenv("CLIENT_ID"),
            "BW_CLIENTSECRET="+os.Getenv("CLIENT_SECRET"))
        loginOutput, loginErr := loginCmd.CombinedOutput()
        if loginErr != nil {
            return fmt.Errorf("failed to log in to Bitwarden with API Key: %v\nOutput: %s", loginErr, string(loginOutput))
        }

        unlockCmd := exec.Command("bw", "unlock", "--raw")
        unlockCmd.Stdin = strings.NewReader(os.Getenv("BITWARDEN_MASTER_PASSWORD"))
        sessionKey, unlockErr := unlockCmd.Output()
        if unlockErr != nil {
            return fmt.Errorf("failed to unlock Bitwarden after login: %v", unlockErr)
        }
        os.Setenv("BW_SESSION", strings.TrimSpace(string(sessionKey)))
    }

    return nil
}

func handleProfileCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
    err := ensureBitwardenLogin()
    if err != nil {
        log.Printf("Bitwarden login failed: %v", err)
        return
    }

    err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: "Processing your request...",
        },
    })
    if err != nil {
        log.Printf("Failed to send initial response: %v", err)
        return
    }

    attachmentID := i.ApplicationCommandData().Options[0].Value.(string)
    csvAttachment, ok := i.ApplicationCommandData().Resolved.Attachments[attachmentID]
    if !ok {
        log.Printf("Failed to retrieve CSV attachment")
        return
    }
    csvFileURL := csvAttachment.URL

    response, err := http.Get(csvFileURL)
    if err != nil {
        log.Printf("Failed to download CSV file: %v", err)
        return
    }
    defer response.Body.Close()

    tempFile, err := os.CreateTemp("", "profile_data_*.csv")
    if err != nil {
        log.Printf("Failed to create temp file: %v", err)
        return
    }
    defer os.Remove(tempFile.Name())

    _, err = io.Copy(tempFile, response.Body)
    if err != nil {
        log.Printf("Failed to save CSV file: %v", err)
        return
    }

    file, err := os.Open(tempFile.Name())
    if err != nil {
        log.Printf("Failed to open CSV file: %v", err)
        return
    }
    defer file.Close()

    csvReader := csv.NewReader(file)
    rows, err := csvReader.ReadAll()
    if err != nil {
        log.Printf("Failed to read CSV file: %v", err)
        return
    }

    header := rows[0]
    colIndex := map[string]int{
        "name":           -1,
        "notes":          -1,
        "login_username": -1,
        "login_password": -1,
        "login_uri":      -1,
    }

    for idx, colName := range header {
        if _, ok := colIndex[colName]; ok {
            colIndex[colName] = idx
        }
    }

    for key, idx := range colIndex {
        if idx == -1 {
            log.Printf("Missing required column: %s", key)
            return
        }
    }

    jar, _ := cookiejar.New(nil)
    client := &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
        },
        Jar: jar,
    }

    loginSuccess, err := loginToPfSense(client)
    if err != nil || !loginSuccess {
        log.Printf("Login to pfSense failed: %v", err)
        return
    }

    for i, row := range rows {
        if i == 0 {
            continue
        }
        newUsername := row[colIndex["login_username"]]
        newPassword := row[colIndex["login_password"]]
        descr := row[colIndex["name"]]
        discordHandle := row[colIndex["notes"]]

        err := createUser(client, newUsername, newPassword, descr)
        if err != nil {
            log.Printf("VPN profile creation failed for %s: %v", newUsername, err)
            continue
        }
        log.Printf("VPN profile for %s was created successfully.", newUsername)

        userID, err := getUserIDByUsername(s, GuildID, discordHandle)
        if err != nil {
            log.Printf("Failed to find user ID for %s: %v", discordHandle, err)
            continue
        }

        err = notifyUserOnDiscord(s, userID, newUsername, newPassword)
        if err != nil {
            log.Printf("Failed to notify %s: %v", discordHandle, err)
        }
    }

    cmd := exec.Command("bw", "import", "bitwarden", tempFile.Name())
    cmd.Env = append(os.Environ(), "BW_SESSION="+os.Getenv("BW_SESSION"))
    output, err := cmd.CombinedOutput()
    if err != nil {
        log.Printf("Failed to import CSV into Bitwarden: %v\nOutput: %s", err, string(output))
        return
    }
    log.Printf("Successfully imported CSV into Bitwarden")

    listCmd := exec.Command("bw", "list", "items")
    listCmd.Env = append(os.Environ(), "BW_SESSION="+os.Getenv("BW_SESSION"))
    listOutput, err := listCmd.CombinedOutput()
    if err != nil {
        log.Printf("Failed to list items: %v\nOutput: %s", err, string(listOutput))
        return
    }

    items := parseItems(listOutput)

    for _, itemID := range items {
        moveCmd := exec.Command("bw", "edit", "item", itemID, `{"organizationId": "28e94aa1-83c7-4251-973b-b22601688e21"}`)
        moveCmd.Env = append(os.Environ(), "BW_SESSION="+os.Getenv("BW_SESSION"))
        moveOutput, err := moveCmd.CombinedOutput()
        if err != nil {
            log.Printf("Failed to move item %s to organization: %v\nOutput: %s", itemID, err, string(moveOutput))
        } else {
            log.Printf("Successfully moved item %s to organization", itemID)
        }
    }

    _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
        Content: "All VPN profiles were created successfully, and items were moved to SOC/SDC.",
    })
    if err != nil {
        log.Printf("Failed to send follow-up message: %v", err)
    }
}

func parseItems(jsonData []byte) []string {
    var items []map[string]interface{}
    var itemIDs []string

    err := json.Unmarshal(jsonData, &items)
    if err != nil {
        log.Printf("Failed to parse items JSON: %v", err)
        return itemIDs
    }

    for _, item := range items {
        if id, ok := item["id"].(string); ok {
            itemIDs = append(itemIDs, id)
        }
    }

    return itemIDs
}

func handleSingleProfileCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
    err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: "Processing your request...",
        },
    })
    if err != nil {
        log.Printf("Failed to send initial response: %v", err)
        return
    }

    newUsername := i.ApplicationCommandData().Options[0].StringValue()
    newPassword := i.ApplicationCommandData().Options[1].StringValue()
    descr := i.ApplicationCommandData().Options[2].StringValue()
    discordHandle := i.ApplicationCommandData().Options[3].StringValue()

    jar, _ := cookiejar.New(nil)
    client := &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
        },
        Jar: jar,
    }

    loginSuccess, err := loginToPfSense(client)
    if err != nil || !loginSuccess {
        log.Printf("Login to pfSense failed: %v", err)
    }

    err = createUser(client, newUsername, newPassword, descr)
    if err != nil {
        log.Printf("VPN profile creation failed for %s: %v", newUsername, err)
    } else {
        log.Printf("VPN profile for %s was created successfully.", newUsername)
    }

    userID, err := getUserIDByUsername(s, GuildID, discordHandle)
    if err != nil {
        log.Printf("Failed to find user ID for %s: %v", discordHandle, err)
    }

    if userID != "" {
        err = notifyUserOnDiscord(s, userID, newUsername, newPassword)
        if err != nil {
            log.Printf("Failed to notify %s: %v", discordHandle, err)
        }
    }

    _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
        Content: "VPN profile was created successfully for " + newUsername,
    })
    if err != nil {
        log.Printf("Failed to send follow-up message: %v", err)
    }
}

func handleDisplayCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
    err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: "Fetching the user list...",
        },
    })
    if err != nil {
        log.Printf("Failed to send initial response: %v", err)
        return
    }

    jar, _ := cookiejar.New(nil)
    client := &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
        },
        Jar: jar,
    }

    loginSuccess, err := loginToPfSense(client)
    if err != nil || !loginSuccess {
        respondWithError(s, i, "Login to pfSense failed. Please try again.")
        return
    }

    userManagerURL := BaseURL + UserManagerPath
    resp, err := client.Get(userManagerURL)
    if err != nil {
        respondWithError(s, i, "Failed to fetch user manager page.")
        return
    }
    defer resp.Body.Close()

    doc, err := goquery.NewDocumentFromReader(resp.Body)
    if err != nil {
        respondWithError(s, i, "Failed to parse user manager page.")
        return
    }

    var userList []string

    id := 0
    doc.Find("tr").Each(func(index int, row *goquery.Selection) {
        username := strings.TrimSpace(row.Find("td").Eq(1).Text())
        fullName := strings.TrimSpace(row.Find("td").Eq(2).Text())
        if username != "" && fullName != "" {
            userList = append(userList, fmt.Sprintf("ID: %d - %s - %s", id, username, fullName))
            id++
        }
    })

    messageContent := strings.Join(userList, "\n")

    if len(messageContent) > 2000 {
        log.Printf("User list output (over character limit):\n%s", messageContent)
        _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
            Content: "Output over character limit, check logs",
        })
    } else {
        _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
            Content: messageContent,
        })
    }

    if err != nil {
        log.Printf("Failed to send message: %v", err)
    }
}

func handleDeleteCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
    err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: "Processing delete request...",
        },
    })
    if err != nil {
        log.Printf("Failed to send initial response: %v", err)
        return
    }

    userIDs := i.ApplicationCommandData().Options[0].StringValue()
    ids := strings.Split(userIDs, ",")

    jar, _ := cookiejar.New(nil)
    client := &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
        },
        Jar: jar,
    }

    loginSuccess, err := loginToPfSense(client)
    if err != nil || !loginSuccess {
        respondWithError(s, i, "Login to pfSense failed. Please try again.")
        return
    }

    err = deleteUser(client, ids)
    if err != nil {
        respondWithError(s, i, fmt.Sprintf("Failed to delete users: %v", err))
        return
    }

    _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
        Content: "Users deleted successfully.",
    })
    if err != nil {
        log.Printf("Failed to send follow-up message: %v", err)
    }
}

func handleStatusCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
    err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: "I hate you (bot is online)",
        },
    })
    if err != nil {
        log.Printf("Failed to send status response: %v", err)
    }
}

func handleKillCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
    err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
        Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
        Data: &discordgo.InteractionResponseData{
            Content: "Terminating VPN session...",
        },
    })
    if err != nil {
        log.Printf("Failed to send initial response: %v", err)
        return
    }

    username := i.ApplicationCommandData().Options[0].StringValue()
    log.Println("Attempting to terminate session for:", username)

    jar, _ := cookiejar.New(nil)
    client := &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
        },
        Jar: jar,
    }

    loginSuccess, err := loginToPfSense(client)
    if err != nil || !loginSuccess {
        respondWithError(s, i, "Login to pfSense failed. Please try again.")
        return
    }

    vpnStatusURL := BaseURL + OpenvpnStatusPath
    resp, err := client.Get(vpnStatusURL)
    if err != nil {
        respondWithError(s, i, "Failed to fetch OpenVPN status page.")
        return
    }
    defer resp.Body.Close()

    doc, err := goquery.NewDocumentFromReader(resp.Body)
    if err != nil {
        respondWithError(s, i, "Failed to parse OpenVPN status page.")
        return
    }

    var clientIPPort string
    doc.Find("tr").Each(func(index int, row *goquery.Selection) {
        clientName := strings.TrimSpace(row.Find("td").Eq(0).Text())
        clientIP := strings.TrimSpace(row.Find("td").Eq(1).Text())

        if strings.Contains(clientName, username) && clientIP != "" {
            clientIPPort = clientIP
            log.Printf("Match found for username: %s with clientIPPort: %s", clientName, clientIPPort)
        }
    })

    if clientIPPort == "" {
        message := fmt.Sprintf("Session for %s not found.", username)
        log.Println(message)
        _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
            Content: message,
        })
        if err != nil {
            log.Printf("Failed to send follow-up message: %v", err)
        }
        return
    }

    log.Printf("Calling terminateSession with clientIPPort: %s", clientIPPort)
    err = terminateSession(client, "server1", clientIPPort)
    if err != nil {
        respondWithError(s, i, fmt.Sprintf("Failed to terminate session: %v", err))
        return
    }

    _, err = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
        Content: fmt.Sprintf("VPN session for %s terminated successfully.", username),
    })
    if err != nil {
        log.Printf("Failed to send follow-up message: %v", err)
    }
}
