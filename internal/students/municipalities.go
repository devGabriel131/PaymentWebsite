package students

import "strings"

// Source: https://pr.gov/gobierno-2/directorio-municipios
var municipalities = []string{
	"Adjuntas",
	"Aguada",
	"Aguadilla",
	"Aguas Buenas",
	"Aibonito",
	"Añasco",
	"Arecibo",
	"Arroyo",
	"Barceloneta",
	"Barranquitas",
	"Bayamón",
	"Cabo Rojo",
	"Caguas",
	"Camuy",
	"Canóvanas",
	"Carolina",
	"Cataño",
	"Cayey",
	"Ceiba",
	"Ciales",
	"Cidra",
	"Coamo",
	"Comerío",
	"Corozal",
	"Culebra",
	"Dorado",
	"Fajardo",
	"Florida",
	"Guánica",
	"Guayama",
	"Guayanilla",
	"Guaynabo",
	"Gurabo",
	"Hatillo",
	"Hormigueros",
	"Humacao",
	"Isabela",
	"Jayuya",
	"Juana Díaz",
	"Juncos",
	"Lajas",
	"Lares",
	"Las Marías",
	"Las Piedras",
	"Loíza",
	"Luquillo",
	"Manatí",
	"Maricao",
	"Maunabo",
	"Mayagüez",
	"Moca",
	"Morovis",
	"Naguabo",
	"Naranjito",
	"Orocovis",
	"Patillas",
	"Peñuelas",
	"Ponce",
	"Quebradillas",
	"Rincón",
	"Río Grande",
	"Sabana Grande",
	"Salinas",
	"San Germán",
	"San Juan",
	"San Lorenzo",
	"San Sebastián",
	"Santa Isabel",
	"Toa Alta",
	"Toa Baja",
	"Trujillo Alto",
	"Utuado",
	"Vega Alta",
	"Vega Baja",
	"Vieques",
	"Villalba",
	"Yabucoa",
	"Yauco",
	"Fuera de Puerto Rico",
}

// MunicipalityOptions returns the ordered choices. Callers must not modify the slice.
func MunicipalityOptions() []string {
	return municipalities
}

func ParseMunicipality(raw string, required bool) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" && !required {
		return "", nil
	}
	for _, municipality := range municipalities {
		if value == municipality {
			return value, nil
		}
	}
	return "", &ValidationError{Message: "Select a municipality or Fuera de Puerto Rico."}
}
