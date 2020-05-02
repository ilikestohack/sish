package sshmuxer

import (
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antoniomika/sish/httpmuxer"
	"github.com/antoniomika/sish/utils"
	"github.com/jpillora/ipfilter"
	"github.com/spf13/viper"
	"golang.org/x/crypto/ssh"
)

var (
	httpPort  int
	httpsPort int
	filter    *ipfilter.IPFilter
)

// Start initializes the ssh muxer service
func Start() {
	_, httpPortString, err := net.SplitHostPort(viper.GetString("http-address"))
	if err != nil {
		log.Fatalln("Error parsing address:", err)
	}

	_, httpsPortString, err := net.SplitHostPort(viper.GetString("https-address"))
	if err != nil {
		log.Fatalln("Error parsing address:", err)
	}

	httpPort, err = strconv.Atoi(httpPortString)
	if err != nil {
		log.Fatalln("Error parsing address:", err)
	}

	httpsPort, err = strconv.Atoi(httpsPortString)
	if err != nil {
		log.Fatalln("Error parsing address:", err)
	}

	if viper.GetInt("http-port-override") != 0 {
		httpPort = viper.GetInt("http-port-override")
	}

	if viper.GetInt("https-port-override") != 0 {
		httpsPort = viper.GetInt("https-port-override")
	}

	upperList := func(stringList string) []string {
		list := strings.FieldsFunc(stringList, utils.CommaSplitFields)
		for k, v := range list {
			list[k] = strings.ToUpper(v)
		}

		return list
	}

	whitelistedCountriesList := upperList(viper.GetString("whitelisted-countries"))
	whitelistedIPList := strings.FieldsFunc(viper.GetString("whitelisted-ips"), utils.CommaSplitFields)

	ipfilterOpts := ipfilter.Options{
		BlockedCountries: upperList(viper.GetString("banned-countries")),
		AllowedCountries: whitelistedCountriesList,
		BlockedIPs:       strings.FieldsFunc(viper.GetString("banned-ips"), utils.CommaSplitFields),
		AllowedIPs:       whitelistedIPList,
		BlockByDefault:   len(whitelistedIPList) > 0 || len(whitelistedCountriesList) > 0,
	}

	if viper.GetBool("enable-geodb") {
		filter = ipfilter.NewLazy(ipfilterOpts)
	} else {
		filter = ipfilter.NewNoDB(ipfilterOpts)
	}

	utils.WatchCerts()

	state := &utils.State{
		SSHConnections: &sync.Map{},
		Listeners:      &sync.Map{},
		HTTPListeners:  &sync.Map{},
		TCPListeners:   &sync.Map{},
		IPFilter:       filter,
		Console:        utils.NewWebConsole(),
	}

	state.Console.State = state

	go httpmuxer.StartHTTPHandler(state)

	if viper.GetBool("debug") {
		go func() {
			for {
				log.Println("=======Start=========")
				log.Println("===Goroutines=====")
				log.Println(runtime.NumGoroutine())
				log.Println("===Listeners======")
				state.Listeners.Range(func(key, value interface{}) bool {
					log.Println(key, value)
					return true
				})
				log.Println("===Clients========")
				state.SSHConnections.Range(func(key, value interface{}) bool {
					log.Println(key, value)
					return true
				})
				log.Println("===HTTP Clients===")
				state.HTTPListeners.Range(func(key, value interface{}) bool {
					log.Println(key, value)
					return true
				})
				log.Println("===TCP Aliases====")
				state.TCPListeners.Range(func(key, value interface{}) bool {
					log.Println(key, value)
					return true
				})
				log.Println("===Web Console Routes====")
				state.Console.Clients.Range(func(key, value interface{}) bool {
					log.Println(key, value)
					return true
				})
				log.Println("===Web Console Tokens====")
				state.Console.RouteTokens.Range(func(key, value interface{}) bool {
					log.Println(key, value)
					return true
				})
				log.Print("========End==========\n\n")

				time.Sleep(2 * time.Second)
			}
		}()
	}

	log.Println("Starting SSH service on address:", viper.GetString("ssh-address"))

	sshConfig := utils.GetSSHConfig()

	listener, err := net.Listen("tcp", viper.GetString("ssh-address"))
	if err != nil {
		log.Fatal(err)
	}

	state.Listeners.Store(listener.Addr(), listener)

	defer func() {
		listener.Close()
		state.Listeners.Delete(listener.Addr())
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			os.Exit(0)
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println(err)
			continue
		}

		clientRemote, _, err := net.SplitHostPort(conn.RemoteAddr().String())

		if err != nil || filter.Blocked(clientRemote) {
			conn.Close()
			continue
		}

		clientLoggedIn := false

		if viper.GetBool("cleanup-unbound") {
			go func() {
				<-time.After(5 * time.Second)
				if !clientLoggedIn {
					conn.Close()
				}
			}()
		}

		log.Println("Accepted SSH connection for:", conn.RemoteAddr())

		go func() {
			sshConn, chans, reqs, err := ssh.NewServerConn(conn, sshConfig)
			clientLoggedIn = true
			if err != nil {
				conn.Close()
				log.Println(err)
				return
			}

			holderConn := &utils.SSHConnection{
				SSHConn:   sshConn,
				Listeners: &sync.Map{},
				Close:     make(chan bool),
				Messages:  make(chan string),
				Session:   make(chan bool),
			}

			state.SSHConnections.Store(sshConn.RemoteAddr(), holderConn)

			go func() {
				err := sshConn.Wait()
				if err != nil && viper.GetBool("debug") {
					log.Println("Closing SSH connection:", err)
				}

				select {
				case <-holderConn.Close:
					break
				default:
					holderConn.CleanUp(state)
				}
			}()

			go handleRequests(reqs, holderConn, state)
			go handleChannels(chans, holderConn, state)

			if viper.GetBool("cleanup-unbound") {
				go func() {
					select {
					case <-time.After(1 * time.Second):
						count := 0
						holderConn.Listeners.Range(func(key, value interface{}) bool {
							count++
							return true
						})

						if count == 0 {
							holderConn.SendMessage("No forwarding requests sent. Closing connection.", true)
							time.Sleep(1 * time.Millisecond)
							holderConn.CleanUp(state)
						}
					case <-holderConn.Close:
						return
					}
				}()
			}

			if viper.GetBool("ping-client") {
				go func() {
					tickDuration := viper.GetDuration("ping-client-interval")
					ticker := time.NewTicker(tickDuration)

					for {
						err := conn.SetDeadline(time.Now().Add(tickDuration).Add(viper.GetDuration("connection-idle-timeout")))
						if err != nil {
							log.Println("Unable to set deadline")
						}

						select {
						case <-ticker.C:
							_, _, err := sshConn.SendRequest("keepalive@sish", true, nil)
							if err != nil {
								log.Println("Error retrieving keepalive response")
								return
							}
						case <-holderConn.Close:
							return
						}
					}
				}()
			}
		}()
	}
}
