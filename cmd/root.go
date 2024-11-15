package cmd

import (
	"fmt"
	"go-meshtastic/app"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rootCmd = &cobra.Command{
	Use:   "go-meshtastic",
	Short: "go-meshtastic server.",
	Long:  `go-meshtastic server connects to an mqtt bus, decodes Meshtastic Protobuf messages, and sends them to influx.`,
	Run: func(cmd *cobra.Command, args []string) {
		// Run the application
		app.Run()
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().String("mqtt-uri", "tcp://localhost:1883", "MQTT URI")
	rootCmd.PersistentFlags().String("mqtt-username", "", "MQTT Username")
	rootCmd.PersistentFlags().String("mqtt-password", "", "MQTT Password")
	rootCmd.PersistentFlags().String("influx-uri", "http://localhost:8086", "InfluxDB URI")
	rootCmd.PersistentFlags().String("userinfo-cache", "./userinfo.cache", "Path to userinfo cache file")

	viper.BindPFlag("mqtt-uri", rootCmd.PersistentFlags().Lookup("mqtt-uri"))
	viper.BindPFlag("mqtt-username", rootCmd.PersistentFlags().Lookup("mqtt-username"))
	viper.BindPFlag("mqtt-password", rootCmd.PersistentFlags().Lookup("mqtt-password"))
	viper.BindPFlag("influx-uri", rootCmd.PersistentFlags().Lookup("influx-uri"))
	viper.BindPFlag("userinfo-cache", rootCmd.PersistentFlags().Lookup("userinfo-cache"))

	viper.SetDefault("config", "./config.yaml")
	rootCmd.PersistentFlags().String("config", "./config.yaml", "config file (default is ./config.yaml)")
	viper.BindPFlag("config", rootCmd.PersistentFlags().Lookup("config"))
}

func initConfig() {
	viper.SetConfigFile(viper.GetString("config"))
	if err := viper.ReadInConfig(); err != nil {
		fmt.Println("Error reading config file:", err)
	}
}
